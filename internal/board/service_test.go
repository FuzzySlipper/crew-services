package board_test

import (
	"context"
	"database/sql"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/board/boardsqlite"
	"crew-services/internal/fabricclient"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

// world is crew-board on a real fabric: two Claude Code sessions, one Codex
// session, and a clock that moves only when told.
type world struct {
	t       *testing.T
	ctx     context.Context
	fabric  *fabricclient.Client
	hub     *service.Service
	dir     string
	store   *boardsqlite.Store
	svc     *board.Service
	now     time.Time
	leases  map[string]fabricclient.Lease
	a, b, x string // session addresses: claude a and b, codex x
	ops     int
}

func newWorld(t *testing.T) *world {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	persistence, err := sqlite.Open(ctx, filepath.Join(dir, "fabric.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistence.Close() })
	fsvc, err := service.New(persistence, service.SystemClock{})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(httpapi.NewHandler(fsvc))
	t.Cleanup(server.Close)
	fabric, err := fabricclient.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	w := &world{t: t, ctx: ctx, fabric: fabric, hub: fsvc, dir: dir, now: time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC), leases: map[string]fabricclient.Lease{}}
	w.a = w.session("claude-code", "a")
	w.b = w.session("claude-code", "b")
	w.x = w.session("crew-codex", "x")
	w.open()
	return w
}

func (w *world) open() {
	w.t.Helper()
	store, err := boardsqlite.Open(w.ctx, filepath.Join(w.dir, "board.db"))
	if err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { _ = store.Close() })
	svc, err := board.New(store, board.FabricDirectory{Fabric: w.fabric}, func() time.Time { return w.now }, board.Config{Site: "den", People: []string{"patch", "lore"}})
	if err != nil {
		w.t.Fatal(err)
	}
	w.store, w.svc = store, svc
}

func (w *world) session(adapter, key string) string {
	w.t.Helper()
	lease, ok := w.leases[adapter]
	if !ok {
		var err error
		if lease, err = w.fabric.Register(w.ctx, adapter, adapter+"-1", time.Minute); err != nil {
			w.t.Fatal(err)
		}
		w.leases[adapter] = lease
	}
	binding, err := w.fabric.RegisterSessionAddress(w.ctx, fabricclient.SessionAddressRequest{AdapterID: adapter, LeaseToken: lease.LeaseToken, NativeKey: key, Capabilities: []string{"deliver_when_idle"}})
	if err != nil {
		w.t.Fatal(err)
	}
	return binding.Address
}

func (w *world) seat(write, seat, adapter, session string) {
	w.t.Helper()
	w.ops++
	if _, err := w.fabric.SeatWrite(w.ctx, write, seat, fabricclient.SeatWriteRequest{AdapterID: adapter, LeaseToken: w.leases[adapter].LeaseToken, OperationID: write + "-" + strings.Repeat("x", w.ops), SessionAddress: session}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) op() string {
	w.ops++
	return "op-" + strings.Repeat("o", w.ops)
}

func (w *world) tick() { w.now = w.now.Add(time.Second) }

func code(err error) board.ErrorCode { return board.CodeOf(err) }

func (w *world) newBoard(name string) {
	w.t.Helper()
	if _, _, err := w.svc.CreateBoard(w.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Name: name}); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) thread(actor board.Actor, title, body string) board.PostResult {
	w.t.Helper()
	w.tick()
	out, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: actor, OperationID: w.op(), Board: "rusty-engine", Title: title, Body: body})
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func (w *world) reply(actor board.Actor, thread, parent, body string) board.PostResult {
	w.t.Helper()
	w.tick()
	out, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: actor, OperationID: w.op(), Thread: thread, Parent: parent, Body: body})
	if err != nil {
		w.t.Fatal(err)
	}
	return out
}

func TestIdentityIsCheckedAgainstTheFabric(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	open := func(actor board.Actor) error {
		_, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: actor, OperationID: w.op(), Board: "rusty-engine", Title: "t", Body: "b"})
		return err
	}
	cases := []struct {
		name  string
		actor board.Actor
		want  board.ErrorCode
	}{
		{"seat held by the caller", board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer"}, ""},
		{"plain session", board.Actor{SessionAddress: w.x}, ""},
		{"person", board.Actor{Person: "lore"}, ""},
		{"seat held by someone else", board.Actor{SessionAddress: w.b, AsSeat: "rusty-engine.reviewer"}, board.CodeNotHolder},
		{"seat that does not exist", board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.nobody"}, board.CodeNotHolder},
		{"unbound session", board.Actor{SessionAddress: "s/0000000000000000"}, board.CodeSessionUnbound},
		{"unknown person", board.Actor{Person: "mallory"}, board.CodeUnknownPerson},
		{"person and session at once", board.Actor{Person: "patch", SessionAddress: w.a}, board.CodeInvalid},
		{"nobody", board.Actor{}, board.CodeInvalid},
	}
	for _, c := range cases {
		if got := code(open(c.actor)); got != c.want {
			t.Errorf("%s: code %q, want %q", c.name, got, c.want)
		}
	}
	// The author of a seat's post is the seat: after relief the new holder may
	// edit it and the old holder may not.
	post := w.thread(board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer"}, "Shader cache", "first draft")
	if post.Post.Author.Kind != board.AuthorSeat || post.Post.Author.Session != w.a || post.Post.Author.Adapter != "claude-code" {
		t.Fatalf("author = %+v", post.Post.Author)
	}
	w.seat("relieve", "rusty-engine.reviewer", "crew-codex", w.x)
	edit := func(actor board.Actor) error {
		_, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: actor, OperationID: w.op(), Post: post.Post.ID, Body: "second draft"})
		return err
	}
	if got := code(edit(board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer"})); got != board.CodeNotHolder {
		t.Fatalf("old holder editing = %q", got)
	}
	if got := code(edit(board.Actor{SessionAddress: w.a})); got != board.CodeNotAuthor {
		t.Fatalf("old holder's own session editing = %q", got)
	}
	if err := edit(board.Actor{SessionAddress: w.x, AsSeat: "rusty-engine.reviewer"}); err != nil {
		t.Fatalf("new holder editing: %v", err)
	}
}

func TestRepliesRevisionsAndPaging(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	opened := w.thread(board.Actor{Person: "patch"}, "Cache layout", "Where do we keep the index?")
	first := w.reply(board.Actor{SessionAddress: w.a}, opened.Thread.ID, "", "Per material.")
	second := w.reply(board.Actor{SessionAddress: w.x}, opened.Thread.ID, first.Post.ID, "Agreed.")
	if first.Post.ParentID != opened.Post.ID || second.Post.ParentID != first.Post.ID {
		t.Fatalf("parents %s %s", first.Post.ParentID, second.Post.ParentID)
	}
	other := w.thread(board.Actor{Person: "lore"}, "Other", "elsewhere")
	if _, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Thread: opened.Thread.ID, Parent: other.Post.ID, Body: "x"}); code(err) != board.CodeParentNotInThread {
		t.Fatalf("parent in another thread = %v", err)
	}
	if _, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Thread: "den-nothing", Body: "x"}); code(err) != board.CodeThreadNotFound {
		t.Fatalf("missing thread = %v", err)
	}

	w.tick()
	if _, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: board.Actor{SessionAddress: w.a}, OperationID: w.op(), Post: first.Post.ID, Body: "Per material, keyed by shader."}); err != nil {
		t.Fatal(err)
	}
	revisions, err := w.svc.Revisions(w.ctx, first.Post.ID)
	if err != nil || len(revisions) != 2 || revisions[0].Body != "Per material." || revisions[1].Body != "Per material, keyed by shader." {
		t.Fatalf("revisions = %+v, %v", revisions, err)
	}
	post, _ := w.svc.GetPost(w.ctx, first.Post.ID)
	if post.Revision != 2 || post.EditedAt.IsZero() || post.Body != "Per material, keyed by shader." {
		t.Fatalf("edited post = %+v", post)
	}

	read, err := w.svc.ReadThread(w.ctx, opened.Thread.ID, "", 2)
	if err != nil || len(read.Posts) != 2 || read.Next != first.Post.ID || read.Thread.Posts != 3 {
		t.Fatalf("first page = %+v, %v", read, err)
	}
	rest, _ := w.svc.ReadThread(w.ctx, opened.Thread.ID, read.Next, 2)
	if len(rest.Posts) != 1 || rest.Posts[0].ID != second.Post.ID || rest.Next != "" {
		t.Fatalf("second page = %+v", rest)
	}

	// Threads list most recently active first, a page at a time.
	page, err := w.svc.ListThreads(w.ctx, "rusty-engine", "", 1, nil, false)
	if err != nil || len(page.Threads) != 1 || page.Threads[0].ID != opened.Thread.ID || page.Next == "" {
		t.Fatalf("threads page 1 = %+v, %v", page, err)
	}
	page, _ = w.svc.ListThreads(w.ctx, "rusty-engine", page.Next, 1, nil, false)
	if len(page.Threads) != 1 || page.Threads[0].ID != other.Thread.ID {
		t.Fatalf("threads page 2 = %+v", page)
	}
	if found, _ := w.svc.Search(w.ctx, "rusty-engine", "keyed by shader", 10); len(found) != 1 || found[0].ID != first.Post.ID {
		t.Fatalf("search = %+v", found)
	}
}

func TestRetractHideAndPurge(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	opened := w.thread(board.Actor{SessionAddress: w.a}, "Secrets", "opening")
	mine := w.reply(board.Actor{SessionAddress: w.a}, opened.Thread.ID, "", "I said a wrong thing")
	theirs := w.reply(board.Actor{SessionAddress: w.x}, opened.Thread.ID, mine.Post.ID, "the token is hunter2")
	moderate := func(f func(context.Context, board.ModerateRequest) (board.PostResult, bool, error), actor board.Actor, post string) error {
		_, _, err := f(w.ctx, board.ModerateRequest{Actor: actor, OperationID: w.op(), Post: post, Note: "oops"})
		return err
	}
	if got := code(moderate(w.svc.Retract, board.Actor{SessionAddress: w.x}, mine.Post.ID)); got != board.CodeNotAuthor {
		t.Fatalf("retracting someone else's post = %q", got)
	}
	if err := moderate(w.svc.Retract, board.Actor{SessionAddress: w.a}, mine.Post.ID); err != nil {
		t.Fatal(err)
	}
	if got := code(moderate(w.svc.Hide, board.Actor{SessionAddress: w.a}, theirs.Post.ID)); got != board.CodePersonOnly {
		t.Fatalf("a session hiding = %q", got)
	}
	if err := moderate(w.svc.Purge, board.Actor{Person: "patch"}, theirs.Post.ID); err != nil {
		t.Fatal(err)
	}
	read, _ := w.svc.ReadThread(w.ctx, opened.Thread.ID, "", 10)
	if len(read.Posts) != 3 || read.Thread.Posts != 2 {
		t.Fatalf("posts after moderation = %+v", read)
	}
	retracted, purged := read.Posts[1], read.Posts[2]
	if retracted.State != board.PostRetracted || retracted.Body != "" || retracted.Note != "oops" {
		t.Fatalf("retracted = %+v", retracted)
	}
	if purged.State != board.PostPurged || purged.Body != "" || purged.Author.Address != "" || purged.ParentID != mine.Post.ID {
		t.Fatalf("purged tombstone = %+v", purged)
	}
	if _, err := w.svc.Revisions(w.ctx, theirs.Post.ID); code(err) != board.CodePostNotFound {
		t.Fatalf("revisions of a purged post = %v", err)
	}
	if found, _ := w.svc.Search(w.ctx, "rusty-engine", "hunter2", 10); len(found) != 0 {
		t.Fatalf("search found purged text: %+v", found)
	}
	revisions, _ := w.store.ListRevisions(w.ctx, theirs.Post.ID)
	if len(revisions) != 1 || revisions[0].Body != "" {
		t.Fatalf("purged text left in the store: %+v", revisions)
	}
	// Purging the opening post purges the thread.
	if err := moderate(w.svc.Purge, board.Actor{Person: "lore"}, opened.Post.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := w.svc.ReadThread(w.ctx, opened.Thread.ID, "", 10); code(err) != board.CodeThreadNotFound {
		t.Fatalf("purged thread read = %v", err)
	}
	if page, _ := w.svc.ListThreads(w.ctx, "rusty-engine", "", 10, nil, false); len(page.Threads) != 0 {
		t.Fatalf("purged thread still listed: %+v", page)
	}
}

func TestMentionsResolveOnTheAuthorsSite(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	body := "Ping @rusty-engine.reviewer, @" + w.x + " and @lore; also @codex, @rusty-engine.nobody, @kate@interlake and `@patch`."
	out := w.thread(board.Actor{Person: "patch"}, "Mentions", body)
	var resolved, warned []string
	for _, m := range out.Post.Mentions {
		resolved = append(resolved, m.Kind+":"+m.Address+"@"+m.Site)
	}
	for _, m := range out.Warnings {
		warned = append(warned, m.Text)
	}
	if strings.Join(resolved, ",") != "seat:rusty-engine.reviewer@den,session:"+w.x+"@den,person:lore@den" {
		t.Fatalf("resolved = %v", resolved)
	}
	if strings.Join(warned, ",") != "@codex,@rusty-engine.nobody,@kate@interlake" {
		t.Fatalf("warned = %v", warned)
	}
	stored, _ := w.svc.GetPost(w.ctx, out.Post.ID)
	if len(stored.Mentions) != 3 {
		t.Fatalf("stored mentions = %+v", stored.Mentions)
	}
}

func TestWritesAreIdempotentAndSurviveRestart(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	request := board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: "open-once", Board: "rusty-engine", Title: "Once", Body: "only once"}
	first, replayed, err := w.svc.OpenThread(w.ctx, request)
	if err != nil || replayed {
		t.Fatal(err, replayed)
	}
	again, replayed, err := w.svc.OpenThread(w.ctx, request)
	if err != nil || !replayed || again.Thread.ID != first.Thread.ID {
		t.Fatalf("replay = %+v %v %v", again, replayed, err)
	}
	request.Body = "different"
	if _, _, err := w.svc.OpenThread(w.ctx, request); code(err) != board.CodeOperationConflict {
		t.Fatalf("reused operation = %v", err)
	}
	if _, _, err := w.svc.CreateBoard(w.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Name: "Bad Name"}); code(err) != board.CodeInvalidBoardName {
		t.Fatalf("bad board name = %v", err)
	}
	if err := w.store.Close(); err != nil {
		t.Fatal(err)
	}
	w.open()
	read, err := w.svc.ReadThread(w.ctx, first.Thread.ID, "", 10)
	if err != nil || len(read.Posts) != 1 || read.Posts[0].Body != "only once" {
		t.Fatalf("after restart = %+v, %v", read, err)
	}
	if _, replayed, _ := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: "open-once", Board: "rusty-engine", Title: "Once", Body: "only once"}); !replayed {
		t.Fatal("receipt lost across restart")
	}
}

// operationsMention counts stored receipts whose result contains text.
func (w *world) operationsMention(text string) int {
	w.t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(w.dir, "board.db"))
	if err != nil {
		w.t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM operations WHERE result LIKE ?`, "%"+text+"%").Scan(&n); err != nil {
		w.t.Fatal(err)
	}
	return n
}

// A purge scrubs the receipts of the writes it undoes: their stored results
// lose the text, and retrying the original operation is refused instead of
// replaying it or writing again, also after a restart.
func TestPurgeScrubsOperationReceipts(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	opened := w.thread(board.Actor{Person: "patch"}, "Keep me", "keep this opening")
	replyRequest := board.ReplyRequest{Actor: board.Actor{SessionAddress: w.x}, OperationID: "reply-once", Thread: opened.Thread.ID, Body: "purge-me: the token is hunter2"}
	reply, _, err := w.svc.Reply(w.ctx, replyRequest)
	if err != nil {
		t.Fatal(err)
	}
	if w.operationsMention("hunter2") == 0 {
		t.Fatal("the test expects the reply receipt to hold its text before the purge")
	}
	if _, _, err := w.svc.Purge(w.ctx, board.ModerateRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Post: reply.Post.ID}); err != nil {
		t.Fatal(err)
	}
	if n := w.operationsMention("hunter2"); n != 0 {
		t.Fatalf("%d receipts still hold purged text", n)
	}
	if _, _, err := w.svc.Reply(w.ctx, replyRequest); board.CodeOf(err) != board.CodePostNotFound {
		t.Fatalf("retrying a purged reply = %v", err)
	}
	if w.operationsMention("keep this opening") == 0 {
		t.Fatal("an unrelated receipt was scrubbed")
	}

	openRequest := board.OpenThreadRequest{Actor: board.Actor{Person: "lore"}, OperationID: "open-once", Board: "rusty-engine", Title: "Secret title", Body: "purge-me-too"}
	secret, _, err := w.svc.OpenThread(w.ctx, openRequest)
	if err != nil {
		t.Fatal(err)
	}
	w.tick()
	if _, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: board.Actor{Person: "lore"}, OperationID: "edit-once", Post: secret.Post.ID, Body: "purge-me-three"}); err != nil {
		t.Fatal(err)
	}
	w.reply(board.Actor{Person: "patch"}, secret.Thread.ID, "", "a reply in the secret thread")
	if _, _, err := w.svc.Purge(w.ctx, board.ModerateRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Post: secret.Post.ID}); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"Secret title", "purge-me-too", "purge-me-three", "a reply in the secret thread"} {
		if n := w.operationsMention(text); n != 0 {
			t.Fatalf("%d receipts still hold %q", n, text)
		}
	}
	if err := w.store.Close(); err != nil {
		t.Fatal(err)
	}
	w.open()
	if _, _, err := w.svc.OpenThread(w.ctx, openRequest); board.CodeOf(err) != board.CodePostNotFound {
		t.Fatalf("retrying a purged thread's opening after a restart = %v", err)
	}
}

func TestPostsRecordTheModelTheDriverReported(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	asSeat := w.thread(board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer", Model: "claude-opus-5-5"}, "Models", "first")
	if asSeat.Post.Author.Model != "claude-opus-5-5" {
		t.Fatalf("seat author = %+v", asSeat.Post.Author)
	}
	plain := w.reply(board.Actor{SessionAddress: w.x, Model: " gpt-6-astra "}, asSeat.Thread.ID, "", "second")
	if plain.Post.Author.Model != "gpt-6-astra" {
		t.Fatalf("session author = %+v", plain.Post.Author)
	}
	// A person has no model, and a value that is not a model name is dropped,
	// never refused.
	person := w.reply(board.Actor{Person: "patch", Model: "claude-opus-5-5"}, asSeat.Thread.ID, "", "third")
	junk := w.reply(board.Actor{SessionAddress: w.x, Model: "<b>I am patch</b>"}, asSeat.Thread.ID, "", "fourth")
	if person.Post.Author.Model != "" || junk.Post.Author.Model != "" {
		t.Fatalf("person %+v, junk %+v", person.Post.Author, junk.Post.Author)
	}
	// An edit records the model behind it; the post keeps the one it was
	// written with.
	if _, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: board.Actor{SessionAddress: w.x, Model: "gpt-6.1-sol"}, OperationID: w.op(), Post: plain.Post.ID, Body: "second, edited"}); err != nil {
		t.Fatal(err)
	}
	revisions, err := w.svc.Revisions(w.ctx, plain.Post.ID)
	if err != nil || len(revisions) != 2 || revisions[0].Model != "gpt-6-astra" || revisions[1].Model != "gpt-6.1-sol" {
		t.Fatalf("revisions = %+v, %v", revisions, err)
	}
	if got, _ := w.svc.GetPost(w.ctx, plain.Post.ID); got.Author.Model != "gpt-6-astra" {
		t.Fatalf("edited post's author = %+v", got.Author)
	}
}
