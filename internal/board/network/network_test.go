package network_test

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/board/boardsqlite"
	"crew-services/internal/board/network"
	"crew-services/internal/fabricclient"
	"crew-services/internal/httpapi"
	"crew-services/internal/service"
	"crew-services/internal/sqlite"
)

// site is one crew-board site with its own fabric, store, notifier and
// syncer for the network "friends".
type site struct {
	t       *testing.T
	name    string
	dir     string
	ctx     context.Context
	fabric  *fabricclient.Client
	hub     *service.Service
	store   *boardsqlite.Store
	svc     *board.Service
	notices *board.Notifier
	sync    *network.Syncer
	key     string
	session string
	cfg     network.Config
	ops     int
}

func keygen(t *testing.T, dir, name string) string {
	t.Helper()
	key := filepath.Join(dir, name)
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", key).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v %s", err, out)
	}
	return key
}

func pub(t *testing.T, key string) string {
	t.Helper()
	data, err := os.ReadFile(key + ".pub")
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(data))
}

func newSite(t *testing.T, name, repo, key string, partners []network.Partner, people ...string) *site {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	persistence, err := sqlite.Open(ctx, filepath.Join(dir, "fabric.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = persistence.Close() })
	hub, _ := service.New(persistence, service.SystemClock{})
	server := httptest.NewServer(httpapi.NewHandler(hub))
	t.Cleanup(server.Close)
	fabric, _ := fabricclient.New(server.URL)
	lease, err := fabric.Register(ctx, "claude-code", "c", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := fabric.RegisterSessionAddress(ctx, fabricclient.SessionAddressRequest{AdapterID: "claude-code", LeaseToken: lease.LeaseToken, NativeKey: name + "-session", Capabilities: []string{"deliver_when_idle"}})
	if err != nil {
		t.Fatal(err)
	}
	s := &site{t: t, name: name, dir: dir, ctx: ctx, fabric: fabric, hub: hub, key: key, session: binding.Address}
	s.cfg = network.Config{Name: "friends", Repo: repo, SigningKey: key, Partners: partners, Dir: filepath.Join(dir, "networks", "friends")}
	s.open(people...)
	return s
}

// open (re)opens the site's store and services, as a restart does.
func (s *site) open(people ...string) {
	s.t.Helper()
	store, err := boardsqlite.Open(s.ctx, filepath.Join(s.dir, "board.db"))
	if err != nil {
		s.t.Fatal(err)
	}
	s.t.Cleanup(func() { _ = store.Close() })
	dir := board.FabricDirectory{Fabric: s.fabric}
	svc, err := board.New(store, dir, time.Now, board.Config{Site: s.name, People: people, Networks: []board.NetworkInfo{s.cfg.Info()}})
	if err != nil {
		s.t.Fatal(err)
	}
	s.store, s.svc = store, svc
	s.notices = board.NewNotifier(store, dir, s.fabric, time.Now, board.NotifierConfig{InstanceID: s.name, Site: s.name})
	s.sync = network.NewSyncer(s.cfg, s.name, svc, store, time.Now, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// As Run does when the site starts with the network configured.
	if err := s.sync.Join(s.ctx); err != nil {
		s.t.Fatal(err)
	}
}

func (s *site) op() string {
	s.ops++
	return s.name + "-op-" + strings.Repeat("x", s.ops)
}

func (s *site) pass() {
	s.t.Helper()
	if err := s.sync.Pass(s.ctx); err != nil {
		s.t.Fatalf("%s sync: %v", s.name, err)
	}
}

func (s *site) flags() []string {
	flags, _ := s.store.ListFlags(s.ctx, "friends", 100)
	var out []string
	for _, f := range flags {
		out = append(out, f.Path+": "+f.Reason)
	}
	return out
}

// network makes a bare repo and two sites, den and interlake, pinning each
// other's signing keys.
func twoSites(t *testing.T) (*site, *site, string, string, string) {
	t.Helper()
	for _, tool := range []string{"git", "ssh-keygen"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	keys := t.TempDir()
	repo := filepath.Join(t.TempDir(), "friends.git")
	if out, err := exec.Command("git", "init", "-q", "--bare", "-b", "main", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	denKey, interlakeKey := keygen(t, keys, "den"), keygen(t, keys, "interlake")
	den := newSite(t, "den", repo, denKey, []network.Partner{{Site: "interlake", SigningKey: pub(t, interlakeKey)}}, "patch", "lore")
	interlake := newSite(t, "interlake", repo, interlakeKey, []network.Partner{{Site: "den", SigningKey: pub(t, denKey)}, {Site: "elsewhere", SigningKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIDummyKeyThatNeverSignsAnything000000000 x"}}, "kate")
	return den, interlake, repo, denKey, interlakeKey
}

func TestPostsFlowBothWaysAndMentionsNotifyLocally(t *testing.T) {
	den, interlake, _, _, _ := twoSites(t)
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	opened, _, err := den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "Shared cache layout", Body: "Kate, what do you think? @kate@interlake"})
	if err != nil {
		t.Fatal(err)
	}
	if len(opened.Post.Mentions) != 1 || opened.Post.Mentions[0].Site != "interlake" {
		t.Fatalf("opened = %+v", opened.Post)
	}
	if p, _ := den.svc.GetPost(den.ctx, opened.Post.ID); p.Sync != board.SyncPending {
		t.Fatalf("den's post before its push = %q", p.Sync)
	}
	den.pass()
	if p, _ := den.svc.GetPost(den.ctx, opened.Post.ID); p.Sync != board.SyncSynced {
		t.Fatalf("den's post after its push = %q", p.Sync)
	}
	interlake.pass()
	read, err := interlake.svc.ReadThread(interlake.ctx, opened.Thread.ID, "", 10)
	if err != nil || len(read.Posts) != 1 || !read.Posts[0].Remote || read.Posts[0].Author.Address != "patch" || read.Posts[0].Site != "den" {
		t.Fatalf("interlake's copy = %+v, %v", read, err)
	}
	// The mention of kate is interlake's to deliver; den records nothing for it.
	if n, _ := interlake.svc.Notices(interlake.ctx, "", 10); len(n) != 1 || n[0].Recipient.Address != "kate" || n[0].Recipient.Site != "interlake" {
		t.Fatalf("interlake notices = %+v", n)
	}
	if n, _ := den.svc.Notices(den.ctx, "", 10); len(n) != 0 {
		t.Fatalf("den notified another site's person: %+v", n)
	}

	// kate answers and mentions a den session: den notifies it, no_wake by
	// default for a partner, framed as from outside this box.
	reply, _, err := interlake.svc.Reply(interlake.ctx, board.ReplyRequest{Actor: board.Actor{Person: "kate"}, OperationID: interlake.op(), Thread: opened.Thread.ID, Body: "Per material. @" + den.session + "@den please check."})
	if err != nil {
		t.Fatal(err)
	}
	interlake.pass()
	den.pass()
	got, err := den.svc.GetPost(den.ctx, reply.Post.ID)
	if err != nil || !got.Remote || got.Body != "Per material. @"+den.session+"@den please check." {
		t.Fatalf("den's copy of the reply = %+v, %v", got, err)
	}
	den.notices.Pass(den.ctx)
	notices, _ := den.svc.Notices(den.ctx, "", 10)
	var toSession, toPatch *board.Notice
	for i := range notices {
		switch notices[i].Recipient.Address {
		case den.session:
			toSession = &notices[i]
		case "patch":
			toPatch = &notices[i]
		}
	}
	if toSession == nil || toSession.Policy != "no_wake" || toSession.State != board.NoticeSent {
		t.Fatalf("notice to den's session = %+v", toSession)
	}
	if toPatch == nil || toPatch.Reason != board.ReasonReply {
		t.Fatalf("reply notice to patch = %+v", toPatch)
	}
	deliveries, _ := den.hub.Mailbox(den.ctx, den.session)
	m, err := den.hub.GetMessage(den.ctx, deliveries[len(deliveries)-1].MessageID)
	if err != nil || !strings.Contains(m.Body, "from site interlake, outside this box") || m.ActivationPolicy != "no_wake" {
		t.Fatalf("message to den's session = %+v, %v", m, err)
	}
	if flags := den.flags(); len(flags) != 0 {
		t.Fatalf("den flagged = %v", flags)
	}
}

// commit makes a commit in a third clone of the repo, as a misbehaving or
// mistaken site might, and pushes it.
func commit(t *testing.T, repo, key, path, content string, edit bool) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "rogue")
	run := func(args ...string) {
		t.Helper()
		base := []string{"-c", "user.name=rogue", "-c", "user.email=r@x", "-c", "gpg.format=ssh"}
		if key != "" {
			base = append(base, "-c", "user.signingkey="+key, "-c", "commit.gpgsign=true")
		} else {
			base = append(base, "-c", "commit.gpgsign=false")
		}
		cmd := exec.Command("git", append(base, args...)...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	if out, err := exec.Command("git", "clone", "-q", repo, dir).CombinedOutput(); err != nil {
		t.Fatalf("clone: %v %s", err, out)
	}
	target := filepath.Join(dir, path)
	if edit {
		if _, err := os.Stat(target); err != nil {
			t.Fatalf("editing a file that does not exist: %s", path)
		}
	}
	_ = os.MkdirAll(filepath.Dir(target), 0o700)
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-q", "-m", "rogue")
	run("push", "-q", "origin", "HEAD:refs/heads/main")
}

func TestUntrustedCommitsAreIgnoredAndFlagged(t *testing.T) {
	den, interlake, repo, _, interlakeKey := twoSites(t)
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	opened, _, _ := den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "Trust", Body: "the real opening"})
	den.pass()
	interlake.pass()
	thread := opened.Thread.ID
	postFile := func(id, siteName, body string) string {
		return string(board.EncodePost("design", board.Post{ID: id, ThreadID: thread, Site: siteName, Author: board.Author{Kind: board.AuthorPerson, Address: "kate", Site: siteName}}, board.Revision{Revision: 1, Body: body, CreatedAt: time.Now().UTC()}))
	}
	now := time.Now()
	forgedID, unsignedID, strangerID := board.NewID("den", now), board.NewID("interlake", now), board.NewID("interlake", now.Add(time.Millisecond))
	// interlake signs a post that claims to be den's.
	commit(t, repo, interlakeKey, board.PostFilePath("design", thread, forgedID, 1), postFile(forgedID, "den", "forged as den"), false)
	// An unsigned commit.
	commit(t, repo, "", board.PostFilePath("design", thread, unsignedID, 1), postFile(unsignedID, "interlake", "unsigned"), false)
	// A commit signed by a key nobody pinned.
	stranger := keygen(t, t.TempDir(), "stranger")
	commit(t, repo, stranger, board.PostFilePath("design", thread, strangerID, 1), postFile(strangerID, "interlake", "stranger"), false)
	// interlake, signing properly, edits den's existing post file.
	commit(t, repo, interlakeKey, board.PostFilePath("design", thread, opened.Post.ID, 1), postFile(opened.Post.ID, "den", "rewritten"), true)
	// interlake signs a revision of den's post.
	commit(t, repo, interlakeKey, board.PostFilePath("design", thread, opened.Post.ID, 2), strings.Replace(postFile(opened.Post.ID, "interlake", "revised by interlake"), `"revision": 1`, `"revision": 2`, 1), false)
	den.pass()
	read, _ := den.svc.ReadThread(den.ctx, thread, "", 10)
	if len(read.Posts) != 1 || read.Posts[0].Body != "the real opening" || read.Posts[0].Revision != 1 {
		t.Fatalf("den's thread after the untrusted commits = %+v", read.Posts)
	}
	flags := strings.Join(den.flags(), "\n")
	for _, want := range []string{"claims site den", "not signed by a pinned site key", "changed or deleted", ".r2.md: "} {
		if !strings.Contains(flags, want) {
			t.Errorf("den's flags lack %q:\n%s", want, flags)
		}
	}
	if strings.Count(flags, "not signed by a pinned site key") != 2 {
		t.Errorf("want the unsigned and the stranger's commits flagged:\n%s", flags)
	}
}

func TestARewrittenHistoryIsRefused(t *testing.T) {
	den, interlake, repo, _, interlakeKey := twoSites(t)
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "History", Body: "first"})
	den.pass()
	interlake.pass()
	// Someone with push access rewrites the branch to a fresh history.
	dir := filepath.Join(t.TempDir(), "rewrite")
	for _, args := range [][]string{{"init", "-q", "-b", "main", dir}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("%v %s", err, out)
		}
	}
	os.WriteFile(filepath.Join(dir, "README"), []byte("gone"), 0o600)
	for _, args := range [][]string{{"add", "-A"}, {"-c", "user.name=x", "-c", "user.email=x@x", "-c", "gpg.format=ssh", "-c", "user.signingkey=" + interlakeKey, "commit", "-q", "-S", "-m", "rewrite"}, {"push", "-q", "-f", repo, "HEAD:refs/heads/main"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v %s", args, err, out)
		}
	}
	if err := den.sync.Pass(den.ctx); err == nil || !strings.Contains(err.Error(), "does not extend") {
		t.Fatalf("den's pass over a rewritten history = %v", err)
	}
	st, _ := den.store.GetSyncState(den.ctx, "friends")
	if st.Status != "history_rewritten" {
		t.Fatalf("sync state = %+v", st)
	}
	if err := den.sync.Pass(den.ctx); err == nil {
		t.Fatal("den kept reading after refusing the history")
	}
}

func TestAPushRaceIsResolvedAndAPurgeTravels(t *testing.T) {
	den, interlake, _, _, _ := twoSites(t)
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	opened, _, _ := den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "Race", Body: "start"})
	den.pass()
	interlake.pass()
	fromDen, _, _ := den.svc.Reply(den.ctx, board.ReplyRequest{Actor: board.Actor{Person: "lore"}, OperationID: den.op(), Thread: opened.Thread.ID, Body: "den's reply, pushed in the middle of interlake's"})
	fromInterlake, _, _ := interlake.svc.Reply(interlake.ctx, board.ReplyRequest{Actor: board.Actor{Person: "kate"}, OperationID: interlake.op(), Thread: opened.Thread.ID, Body: "interlake's reply, which loses the race"})
	// den pushes after interlake fetched and committed, before it pushes.
	interlake.sync.SetBeforePush(func() { den.pass() })
	interlake.pass()
	den.pass()
	for _, s := range []*site{den, interlake} {
		read, _ := s.svc.ReadThread(s.ctx, opened.Thread.ID, "", 10)
		if len(read.Posts) != 3 {
			t.Fatalf("%s has %d posts after the race: %+v", s.name, len(read.Posts), read.Posts)
		}
	}
	if p, _ := interlake.svc.GetPost(interlake.ctx, fromInterlake.Post.ID); p.Sync != board.SyncSynced {
		t.Fatalf("interlake's post after the race = %q", p.Sync)
	}

	// A person on den purges den's reply; interlake applies the purge.
	if _, _, err := den.svc.Purge(den.ctx, board.ModerateRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Post: fromDen.Post.ID}); err != nil {
		t.Fatal(err)
	}
	den.pass()
	interlake.pass()
	if p, err := interlake.store.GetPost(interlake.ctx, fromDen.Post.ID); err != nil || p.State != board.PostPurged || p.Body != "" {
		t.Fatalf("interlake's copy after den's purge = %+v, %v", p, err)
	}
}

func TestReindexRebuildsALostIndex(t *testing.T) {
	den, interlake, _, _, _ := twoSites(t)
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	opened, _, _ := den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "Keep", Body: "kept in the repo"})
	den.svc.Reply(den.ctx, board.ReplyRequest{Actor: board.Actor{Person: "lore"}, OperationID: den.op(), Thread: opened.Thread.ID, Body: "a reply for @" + interlake.session + "@interlake"})
	den.pass()
	interlake.pass()
	interlake.notices.Pass(interlake.ctx)
	mail := func() int {
		deliveries, _ := interlake.hub.Mailbox(interlake.ctx, interlake.session)
		return len(deliveries)
	}
	if mail() != 1 {
		t.Fatalf("interlake's session mail after the first read = %d", mail())
	}
	// interlake loses its database.
	if err := interlake.store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(interlake.dir, "board.db")); err != nil {
		t.Fatal(err)
	}
	interlake.open("kate")
	if err := interlake.sync.Reindex(interlake.ctx); err != nil {
		t.Fatal(err)
	}
	read, err := interlake.svc.ReadThread(interlake.ctx, opened.Thread.ID, "", 10)
	if err != nil || len(read.Posts) != 2 || read.Thread.Title != "Keep" {
		t.Fatalf("after reindex = %+v, %v", read, err)
	}
	// Replaying history delivers nothing and wakes nobody.
	interlake.notices.Pass(interlake.ctx)
	if n, _ := interlake.svc.Notices(interlake.ctx, "", 10); len(n) != 0 || mail() != 1 {
		t.Fatalf("reindex made %d notices; session mail = %d", len(n), mail())
	}
	// Reindexing again changes nothing.
	if err := interlake.sync.Reindex(interlake.ctx); err != nil {
		t.Fatal(err)
	}
	if read, _ = interlake.svc.ReadThread(interlake.ctx, opened.Thread.ID, "", 10); len(read.Posts) != 2 {
		t.Fatalf("after a second reindex = %d posts", len(read.Posts))
	}
}

func TestPulledMediaIsCheckedAgainOnTheReceivingSite(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "bwrap", "prlimit"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	den, interlake, _, _, _ := twoSites(t)
	for _, s := range []*site{den, interlake} {
		in, err := board.NewIngester(board.DefaultMediaConfig(filepath.Join(s.dir, "media")), time.Now)
		if err != nil {
			t.Fatal(err)
		}
		s.svc.UseMedia(in, s.store)
	}
	image := filepath.Join(t.TempDir(), "layout.png")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=120x80", "-frames:v", "1", image).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	data, _ := os.ReadFile(image)
	a, err := den.svc.Upload(den.ctx, "layout.png", data)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	opened, _, err := den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "With an image", Body: "![layout](blob:sha256-" + a.Ref + ")"})
	if err != nil {
		t.Fatal(err)
	}
	den.pass()
	interlake.pass()
	got, err := interlake.svc.Attachment(interlake.ctx, a.Ref)
	if err != nil || got.State != board.AttachmentReady || got.Width != 120 || !strings.Contains(strings.Join(got.Checks, ";"), "shared by site den; re-checked here") {
		t.Fatalf("interlake's attachment = %+v, %v", got, err)
	}
	post, _ := interlake.svc.GetPost(interlake.ctx, opened.Post.ID)
	if text := board.RenderForAgentWith(post.Body, false, post.Attachments); !strings.Contains(text, "[image: layout; 120×80; board blob "+a.Ref+"]") {
		t.Fatalf("interlake's render = %q", text)
	}
	if flags := interlake.flags(); len(flags) != 0 {
		t.Fatalf("interlake flagged = %v", flags)
	}

	// A purge on den removes the image on both sites: records, bytes, and the
	// copy den kept of what it pushed.
	_, denPath, _ := den.svc.BlobPath(den.ctx, a.Ref)
	_, interlakePath, _ := interlake.svc.BlobPath(interlake.ctx, a.Ref)
	if _, _, err := den.svc.Purge(den.ctx, board.ModerateRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Post: opened.Post.ID}); err != nil {
		t.Fatal(err)
	}
	den.pass()
	interlake.pass()
	for _, s := range []*site{den, interlake} {
		if _, err := s.svc.Attachment(s.ctx, a.Ref); board.CodeOf(err) != board.CodeAttachmentUnknown {
			t.Errorf("%s still has the attachment: %v", s.name, err)
		}
	}
	for _, path := range []string{denPath, interlakePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("purged image bytes still at %s", path)
		}
	}
	if pending, _ := den.store.PendingOutbox(den.ctx, "friends", 100); len(pending) != 0 {
		t.Errorf("den still has files to push: %+v", pending)
	}
	// Reindexing either site's existing index does not bring the image back,
	// though the repo still holds it.
	for _, s := range []*site{den, interlake} {
		if err := s.sync.Reindex(s.ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := s.svc.Attachment(s.ctx, a.Ref); board.CodeOf(err) != board.CodeAttachmentUnknown {
			t.Errorf("%s has the attachment again after reindex: %v", s.name, err)
		}
	}
	for _, path := range []string{denPath, interlakePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("purged image bytes back at %s after reindex", path)
		}
	}
}

// An image held by the scan and then released by a person is shared under
// the hash of its bytes, and the partner shows it.
func TestAReleasedAttachmentIsSharedSoThePartnerCanShowIt(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "bwrap", "prlimit"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	den, interlake, _, _, _ := twoSites(t)
	held := board.DefaultMediaConfig(filepath.Join(den.dir, "media"))
	held.Scanner = []string{"sh", "-c", "exit 1", "--"}
	in, err := board.NewIngester(held, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	den.svc.UseMedia(in, den.store)
	in2, _ := board.NewIngester(board.DefaultMediaConfig(filepath.Join(interlake.dir, "media")), time.Now)
	interlake.svc.UseMedia(in2, interlake.store)
	image := filepath.Join(t.TempDir(), "flagged.png")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=64x48", "-frames:v", "1", image).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	data, _ := os.ReadFile(image)
	a, err := den.svc.Upload(den.ctx, "flagged.png", data)
	if err != nil || a.State != board.AttachmentHeld {
		t.Fatalf("upload = %+v, %v", a, err)
	}
	released, err := den.svc.Release(den.ctx, board.HeldRequest{Actor: board.Actor{Person: "patch"}, Ref: a.Ref})
	if err != nil || released.State != board.AttachmentReady || released.Ref == released.OutputHash {
		t.Fatalf("release = %+v, %v", released, err)
	}
	if _, _, err := den.svc.CreateBoard(den.ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Name: "design", Network: "friends"}); err != nil {
		t.Fatal(err)
	}
	opened, _, err := den.svc.OpenThread(den.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: den.op(), Board: "design", Title: "Released", Body: "![flagged](blob:sha256-" + a.Ref + ")"})
	if err != nil {
		t.Fatal(err)
	}
	den.pass()
	interlake.pass()
	post, _ := interlake.svc.GetPost(interlake.ctx, opened.Post.ID)
	if text := board.RenderForAgentWith(post.Body, false, post.Attachments); !strings.Contains(text, "[image: flagged; 64×48; board blob "+released.OutputHash+"]") {
		t.Fatalf("interlake's render = %q", text)
	}
	if flags := interlake.flags(); len(flags) != 0 {
		t.Fatalf("interlake flagged = %v", flags)
	}
	// den's own copy still shows it under its local reference.
	local, _ := den.svc.GetPost(den.ctx, opened.Post.ID)
	if text := board.RenderForAgentWith(local.Body, false, local.Attachments); !strings.Contains(text, "[image: flagged; 64×48; board blob "+a.Ref+"]") {
		t.Fatalf("den's render = %q", text)
	}
}
