package board_test

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/fabricclient"
)

// lossyFabric hands the first submit to the fabric but loses its answer, as
// a dropped connection would.
type lossyFabric struct {
	board.NoticeFabric
	mu   sync.Mutex
	lost bool
}

func (f *lossyFabric) Submit(ctx context.Context, r fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error) {
	result, err := f.NoticeFabric.Submit(ctx, r)
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.lost {
		f.lost = true
		return fabricclient.SubmittedMessage{}, errors.New("connection reset after the request was sent")
	}
	return result, err
}

// flakyFabric fails the first failures submits, as an unreachable fabric does.
type flakyFabric struct {
	board.NoticeFabric
	mu       sync.Mutex
	failures int
	submits  int
}

func (f *flakyFabric) Submit(ctx context.Context, r fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error) {
	f.mu.Lock()
	f.submits++
	fail := f.submits <= f.failures
	f.mu.Unlock()
	if fail {
		return fabricclient.SubmittedMessage{}, errors.New("connection refused")
	}
	return f.NoticeFabric.Submit(ctx, r)
}

func (w *world) notifier(fab board.NoticeFabric) *board.Notifier {
	if fab == nil {
		fab = w.fabric
	}
	return board.NewNotifier(w.store, board.FabricDirectory{Fabric: w.fabric}, fab, func() time.Time { return w.now }, board.NotifierConfig{InstanceID: "test"})
}

// mail lists what reached address from crew-board, as "policy|body".
func (w *world) mail(address string) []string {
	w.t.Helper()
	deliveries, err := w.hub.Mailbox(w.ctx, address)
	if err != nil {
		w.t.Fatal(err)
	}
	var out []string
	for _, d := range deliveries {
		m, err := w.hub.GetMessage(w.ctx, d.MessageID)
		if err != nil {
			w.t.Fatal(err)
		}
		if m.SenderAddress == "system/crew-board" {
			out = append(out, m.ActivationPolicy+"|"+m.Body)
		}
	}
	return out
}

func noticesByRecipient(t *testing.T, w *world) map[string]board.Notice {
	t.Helper()
	notices, err := w.svc.Notices(w.ctx, "", 200)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]board.Notice{}
	for _, n := range notices {
		out[n.Recipient.Address+"/"+n.PostID] = n
	}
	return out
}

func TestNoticesGoToMentionsRepliesAndFollowers(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	n := w.notifier(nil)
	// b follows the whole board; the opener follows the thread it opened.
	if _, err := w.svc.Subscribe(w.ctx, board.SubscribeRequest{Actor: board.Actor{SessionAddress: w.b}, Board: "rusty-engine"}); err != nil {
		t.Fatal(err)
	}
	opened := w.thread(board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer"}, "Cache layout", "Where should the index live? @"+w.x+" @patch @rusty-engine.reviewer")
	n.Pass(w.ctx)
	got := noticesByRecipient(t, w)
	if len(got) != 3 {
		t.Fatalf("notices for the opening post = %+v", got)
	}
	for key, want := range map[string]string{w.x + "/" + opened.Post.ID: "mention/sent", "patch/" + opened.Post.ID: "mention/no_route", w.b + "/" + opened.Post.ID: "subscription/sent"} {
		if g := got[key]; g.Reason+"/"+g.State != want {
			t.Errorf("notice %s = %s/%s, want %s", key, g.Reason, g.State, want)
		}
	}
	if _, self := got["rusty-engine.reviewer/"+opened.Post.ID]; self {
		t.Fatal("the author's own seat was notified")
	}
	x := w.mail(w.x)
	if len(x) != 1 || !strings.HasPrefix(x[0], "next_turn|You were mentioned by rusty-engine.reviewer (Claude Code, "+w.a+`) in "Cache layout" (board rusty-engine):`) ||
		!strings.Contains(x[0], "Read it with: crew-codex board read "+opened.Post.ID) || !strings.Contains(x[0], "Answer on the board with: crew-codex board reply "+opened.Post.ID) {
		t.Fatalf("mail to x = %q", x)
	}
	if b := w.mail(w.b); len(b) != 1 || !strings.HasPrefix(b[0], "no_wake|New post in a thread you follow") || strings.Contains(b[0], "Answer on the board") {
		t.Fatalf("mail to b = %q", b)
	}

	// x replies: the seat that wrote the parent hears (as a reply, not as a
	// follower), b follows, and x hears nothing about its own post.
	reply := w.reply(board.Actor{SessionAddress: w.x}, opened.Thread.ID, "", "Per material. --urgent matters here")
	n.Pass(w.ctx)
	got = noticesByRecipient(t, w)
	if g := got["rusty-engine.reviewer/"+reply.Post.ID]; g.Reason != board.ReasonReply || g.State != board.NoticeSent || g.Policy != "next_turn" {
		t.Fatalf("reply notice to the seat = %+v", g)
	}
	if _, self := got[w.x+"/"+reply.Post.ID]; self {
		t.Fatal("the replier was notified of its own post")
	}
	if a := w.mail("rusty-engine.reviewer"); len(a) != 1 || !strings.Contains(a[0], "Reply to your post from "+w.x+" (Codex)") || !strings.Contains(a[0], "crew-claude board read "+reply.Post.ID) {
		t.Fatalf("mail to the seat = %q", a)
	}
	// A notice is sent exactly once: another pass sends nothing more.
	before := len(w.mail(w.b))
	n.Pass(w.ctx)
	if after := len(w.mail(w.b)); after != before {
		t.Fatalf("a second pass resent notices: %d -> %d", before, after)
	}
}

func TestNoticeRetriesAnUnreachableFabricAndRecordsARefusal(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	flaky := &flakyFabric{NoticeFabric: w.fabric, failures: 2}
	n := w.notifier(flaky)
	opened := w.thread(board.Actor{Person: "patch"}, "Retry", "ping @"+w.x)
	for i := 0; i < 3; i++ {
		n.Pass(w.ctx)
		w.now = w.now.Add(10 * time.Minute)
	}
	got := noticesByRecipient(t, w)[w.x+"/"+opened.Post.ID]
	if got.State != board.NoticeSent || got.Attempts != 3 || len(w.mail(w.x)) != 1 {
		t.Fatalf("after an outage: %+v, mail %q", got, w.mail(w.x))
	}

	// A seat retired after the post: the fabric refuses, the notice fails,
	// and the post stands.
	w.seat("take", "rusty-engine.gone", "claude-code", w.a)
	post := w.reply(board.Actor{Person: "patch"}, opened.Thread.ID, "", "for @rusty-engine.gone")
	w.seat("retire", "rusty-engine.gone", "claude-code", w.a)
	n.Pass(w.ctx)
	refusedNotice := noticesByRecipient(t, w)["rusty-engine.gone/"+post.Post.ID]
	if refusedNotice.State != board.NoticeFailed || !strings.Contains(refusedNotice.LastError, "refused") {
		t.Fatalf("notice to a retired seat = %+v", refusedNotice)
	}
	if stored, err := w.svc.GetPost(w.ctx, post.Post.ID); err != nil || stored.State != board.PostVisible {
		t.Fatalf("post after a failed notice = %+v, %v", stored, err)
	}

	// A fabric that stays down: the notice gives up after the retry window.
	down := &flakyFabric{NoticeFabric: w.fabric, failures: 1 << 30}
	n = w.notifier(down)
	late := w.reply(board.Actor{Person: "patch"}, opened.Thread.ID, "", "again @"+w.x)
	n.Pass(w.ctx)
	w.now = w.now.Add(2 * time.Hour)
	n.Pass(w.ctx)
	if g := noticesByRecipient(t, w)[w.x+"/"+late.Post.ID]; g.State != board.NoticeFailed || !strings.Contains(g.LastError, "gave up") {
		t.Fatalf("notice after the retry window = %+v", g)
	}
}

func TestSeatNoticeWaitsForTheNextHolder(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	w.seat("leave", "rusty-engine.reviewer", "claude-code", w.a)
	n := w.notifier(nil)
	w.thread(board.Actor{Person: "lore"}, "For whoever reviews", "@rusty-engine.reviewer please look")
	n.Pass(w.ctx)
	if got := w.mail("rusty-engine.reviewer"); len(got) != 1 {
		t.Fatalf("seat mail = %q", got)
	}
	w.seat("take", "rusty-engine.reviewer", "crew-codex", w.x)
	claim, err := w.fabric.Claim(w.ctx, fabricclient.ClaimRequest{AdapterID: "crew-codex", LeaseToken: w.leases["crew-codex"].LeaseToken, OperationID: "claim", RecipientAddress: "rusty-engine.reviewer", RecipientGeneration: 1, Availability: "idle", ClaimDuration: "1m"})
	if err != nil || !claim.Claimed || claim.HolderAddress != w.x || !strings.Contains(claim.Message.Body, "You were mentioned by lore") {
		t.Fatalf("next holder's claim = %+v, %v", claim, err)
	}
}

func TestReadStateFollowsTheSeatAndReadingChangesNothingElse(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	seat := board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer"}
	opened := w.thread(board.Actor{Person: "patch"}, "Review queue", "@rusty-engine.reviewer first item")
	second := w.reply(board.Actor{Person: "lore"}, opened.Thread.ID, "", "@rusty-engine.reviewer second item")
	w.reply(board.Actor{Person: "patch"}, opened.Thread.ID, "", "an aside nobody is told about")
	unread, err := w.svc.Unread(w.ctx, seat, 10)
	if err != nil || len(unread) != 1 || unread[0].Posts != 2 || unread[0].Mentions != 2 || unread[0].NewestID != second.Post.ID {
		t.Fatalf("unread for the seat = %+v, %v", unread, err)
	}
	noticesBefore, _ := w.svc.Notices(w.ctx, "", 100)
	if err := w.svc.MarkRead(w.ctx, board.MarkReadRequest{Actor: seat, Thread: opened.Thread.ID, Post: opened.Post.ID}); err != nil {
		t.Fatal(err)
	}
	if unread, _ = w.svc.Unread(w.ctx, seat, 10); len(unread) != 1 || unread[0].Posts != 1 {
		t.Fatalf("unread after reading the first post = %+v", unread)
	}
	// The next holder continues from the seat's cursor.
	w.seat("relieve", "rusty-engine.reviewer", "crew-codex", w.x)
	next := board.Actor{SessionAddress: w.x, AsSeat: "rusty-engine.reviewer"}
	if unread, _ = w.svc.Unread(w.ctx, next, 10); len(unread) != 1 || unread[0].NewestID != second.Post.ID || unread[0].Posts != 1 {
		t.Fatalf("unread for the next holder = %+v", unread)
	}
	if _, err := w.svc.Unread(w.ctx, seat, 10); board.CodeOf(err) != board.CodeNotHolder {
		t.Fatalf("relieved holder reading as the seat = %v", err)
	}
	if err := w.svc.MarkRead(w.ctx, board.MarkReadRequest{Actor: next, Thread: opened.Thread.ID, Post: second.Post.ID}); err != nil {
		t.Fatal(err)
	}
	// A cursor never moves back.
	if err := w.svc.MarkRead(w.ctx, board.MarkReadRequest{Actor: next, Thread: opened.Thread.ID, Post: opened.Post.ID}); err != nil {
		t.Fatal(err)
	}
	if unread, _ = w.svc.Unread(w.ctx, next, 10); len(unread) != 0 {
		t.Fatalf("unread after reading everything = %+v", unread)
	}
	// Reading touched no notice.
	noticesAfter, _ := w.svc.Notices(w.ctx, "", 100)
	key := func(ns []board.Notice) []string {
		var out []string
		for _, n := range ns {
			out = append(out, n.ID+n.State)
		}
		sort.Strings(out)
		return out
	}
	if strings.Join(key(noticesBefore), ",") != strings.Join(key(noticesAfter), ",") {
		t.Fatal("reading changed notices")
	}
}

func TestAnEditNotifiesOnlyNewMentions(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	opened := w.thread(board.Actor{SessionAddress: w.a}, "Draft", "cc @"+w.x+" @lore")
	w.tick()
	if _, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: board.Actor{SessionAddress: w.a}, OperationID: w.op(), Post: opened.Post.ID, Body: "cc @" + w.x + " @lore @patch"}); err != nil {
		t.Fatal(err)
	}
	notices, _ := w.svc.Notices(w.ctx, "", 100)
	count := map[string]int{}
	for _, n := range notices {
		count[n.Recipient.Address]++
	}
	if count[w.x] != 1 || count["lore"] != 1 || count["patch"] != 1 {
		t.Fatalf("notices per recipient after an edit = %v", count)
	}
	// A follower newly mentioned by an edit keeps its one notice for the post,
	// and the edit goes through.
	if _, err := w.svc.Subscribe(w.ctx, board.SubscribeRequest{Actor: board.Actor{SessionAddress: w.b}, Thread: opened.Thread.ID}); err != nil {
		t.Fatal(err)
	}
	reply := w.reply(board.Actor{Person: "lore"}, opened.Thread.ID, "", "an update")
	w.tick()
	if _, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: board.Actor{Person: "lore"}, OperationID: w.op(), Post: reply.Post.ID, Body: "an update for @" + w.b}); err != nil {
		t.Fatalf("edit mentioning a follower: %v", err)
	}
}

// A retry after a lost answer submits the very text it first sent, though the
// post was edited and the seat changed hands since: the fabric replays it, and
// the notice ends up sent with one message.
func TestARetryAfterALostAnswerResubmitsTheSameNotice(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	n := w.notifier(&lossyFabric{NoticeFabric: w.fabric})
	opened := w.thread(board.Actor{Person: "patch"}, "Lossy", "first text for @rusty-engine.reviewer")
	n.Pass(w.ctx)
	if g := noticesByRecipient(t, w)["rusty-engine.reviewer/"+opened.Post.ID]; g.State != board.NoticePending || g.Attempts != 1 {
		t.Fatalf("after the lost answer = %+v", g)
	}
	w.tick()
	if _, _, err := w.svc.Edit(w.ctx, board.EditRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Post: opened.Post.ID, Body: "edited text for @rusty-engine.reviewer"}); err != nil {
		t.Fatal(err)
	}
	w.seat("relieve", "rusty-engine.reviewer", "crew-codex", w.x)
	w.now = w.now.Add(time.Minute)
	n.Pass(w.ctx)
	g := noticesByRecipient(t, w)["rusty-engine.reviewer/"+opened.Post.ID]
	mail := w.mail("rusty-engine.reviewer")
	if g.State != board.NoticeSent || g.MessageID == "" || len(mail) != 1 || !strings.Contains(mail[0], "first text") || !strings.Contains(mail[0], "crew-claude board read") {
		t.Fatalf("after the retry: %+v, mail %q", g, mail)
	}
}

// A notice the fabric accepted for a vacant seat that nobody takes in time is
// reported as expired; one still within its TTL stays sent.
func TestANoticeThatOutlivesItsTTLIsReportedExpired(t *testing.T) {
	w := newWorld(t)
	w.now = time.Now().UTC()
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	w.seat("leave", "rusty-engine.reviewer", "claude-code", w.a)
	n := board.NewNotifier(w.store, board.FabricDirectory{Fabric: w.fabric}, w.fabric, func() time.Time { return w.now }, board.NotifierConfig{InstanceID: "test", TTL: time.Second})
	opened := w.thread(board.Actor{Person: "patch"}, "Nobody home", "@rusty-engine.reviewer are you there?")
	n.Pass(w.ctx)
	key := "rusty-engine.reviewer/" + opened.Post.ID
	if g := noticesByRecipient(t, w)[key]; g.State != board.NoticeSent {
		t.Fatalf("after sending = %+v", g)
	}
	n.Pass(w.ctx)
	if g := noticesByRecipient(t, w)[key]; g.State != board.NoticeSent {
		t.Fatalf("checked before its first check is due = %+v", g)
	}
	w.now = w.now.Add(2 * time.Minute)
	n.Pass(w.ctx)
	g := noticesByRecipient(t, w)[key]
	if g.State != board.NoticeExpired || !strings.Contains(g.LastError, "TTL") {
		t.Fatalf("after its TTL = %+v", g)
	}
	if expired, _ := w.svc.Notices(w.ctx, board.NoticeExpired, 10); len(expired) != 1 {
		t.Fatalf("expired notices = %+v", expired)
	}
}
