package board_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
)

func (w *world) threadChange(f func(ctx context.Context, r board.ThreadRequest) (board.Thread, bool, error), r board.ThreadRequest) board.Thread {
	w.t.Helper()
	r.OperationID = w.op()
	w.tick()
	t, _, err := f(w.ctx, r)
	if err != nil {
		w.t.Fatal(err)
	}
	return t
}

func TestSettlingAddsLinksAndNeverLocks(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	opened := w.thread(board.Actor{Person: "patch"}, "Agent memory layout", "How should agents remember?")
	settled := w.threadChange(w.svc.Settle, board.ThreadRequest{Actor: board.Actor{SessionAddress: w.a}, Thread: opened.Thread.ID, Into: "wiki:agent-memory", Note: "Agreed layout"})
	if settled.State != board.ThreadSettled || len(settled.Links) != 1 || settled.Links[0].Ref != "wiki:agent-memory" || settled.Links[0].By.Address != w.a || settled.Ref != "board:den/"+opened.Thread.ID {
		t.Fatalf("settled = %+v", settled)
	}
	later := w.reply(board.Actor{Person: "lore"}, opened.Thread.ID, "", "One more thought after settling.")
	if later.Post.State != board.PostVisible {
		t.Fatalf("post after settling = %+v", later.Post)
	}
	again := w.threadChange(w.svc.Settle, board.ThreadRequest{Actor: board.Actor{Person: "lore"}, Thread: opened.Thread.ID, Into: "den:crew-services/10541"})
	if len(again.Links) != 2 || again.Links[1].Ref != "den:crew-services/10541" {
		t.Fatalf("second settle = %+v", again.Links)
	}
	if _, _, err := w.svc.Settle(w.ctx, board.ThreadRequest{Actor: board.Actor{Person: "lore"}, OperationID: w.op(), Thread: opened.Thread.ID, Into: "ftp://nope"}); board.CodeOf(err) != board.CodeInvalid {
		t.Fatalf("bad link = %v", err)
	}
}

func TestRenameCloseAndArchive(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	first := w.thread(board.Actor{Person: "patch"}, "Shader cache", "start")
	second := w.thread(board.Actor{Person: "patch"}, "Shader cache, again", "continue here")
	renamed := w.threadChange(w.svc.Rename, board.ThreadRequest{Actor: board.Actor{Person: "lore"}, Thread: first.Thread.ID, Title: "Shader cache (round 1)"})
	if renamed.Title != "Shader cache (round 1)" || len(renamed.Aliases) != 1 || renamed.Aliases[0] != "Shader cache" {
		t.Fatalf("renamed = %+v", renamed)
	}
	closed := w.threadChange(w.svc.Close, board.ThreadRequest{Actor: board.Actor{Person: "lore"}, Thread: first.Thread.ID, Redirect: second.Thread.ID})
	if closed.State != board.ThreadClosed || closed.RedirectTo != second.Thread.ID {
		t.Fatalf("closed = %+v", closed)
	}
	if _, _, err := w.svc.Close(w.ctx, board.ThreadRequest{Actor: board.Actor{Person: "lore"}, OperationID: w.op(), Thread: second.Thread.ID, Redirect: second.Thread.ID}); board.CodeOf(err) != board.CodeInvalid {
		t.Fatalf("redirect to itself = %v", err)
	}
	w.reply(board.Actor{Person: "patch"}, first.Thread.ID, "", "posting to a closed thread still works")

	open, _ := w.svc.ListThreads(w.ctx, "rusty-engine", "", 10, []string{board.ThreadOpen}, false)
	if len(open.Threads) != 1 || open.Threads[0].ID != second.Thread.ID {
		t.Fatalf("open threads = %+v", open.Threads)
	}
	// A month later both have gone quiet: the default listing leaves them out,
	// --archived shows them, and they are still readable.
	w.now = w.now.Add(31 * 24 * time.Hour)
	if page, _ := w.svc.ListThreads(w.ctx, "rusty-engine", "", 10, nil, false); len(page.Threads) != 0 {
		t.Fatalf("quiet threads still listed: %+v", page.Threads)
	}
	if page, _ := w.svc.ListThreads(w.ctx, "rusty-engine", "", 10, nil, true); len(page.Threads) != 2 {
		t.Fatalf("archived listing = %+v", page.Threads)
	}
	if read, err := w.svc.ReadThread(w.ctx, first.Thread.ID, "", 10); err != nil || len(read.Posts) != 2 {
		t.Fatalf("archived thread read = %+v, %v", read, err)
	}
	// The old title still finds the thread.
	if found, _ := w.svc.Search(w.ctx, "rusty-engine", "Shader cache", 10); len(found) == 0 {
		t.Fatal("search by an old title found nothing")
	}
}

func TestDecisionsAreAcknowledgedBySeatAcrossAHandover(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.seat("take", "rusty-engine.reviewer", "claude-code", w.a)
	opened := w.thread(board.Actor{Person: "patch"}, "Cache policy", "Proposal")
	w.tick()
	decision, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Thread: opened.Thread.ID, Kind: "Decision", Body: "We keep the index per material. @rusty-engine.reviewer @lore please acknowledge."})
	if err != nil || decision.Post.Kind != board.KindDecision {
		t.Fatalf("decision = %+v, %v", decision.Post, err)
	}
	waiting, _ := w.svc.OpenAcks(w.ctx, "rusty-engine", 10)
	if len(waiting) != 1 || len(waiting[0].Waiting) != 2 {
		t.Fatalf("open acks = %+v", waiting)
	}
	// Someone the decision does not name cannot acknowledge it.
	if _, err := w.svc.Ack(w.ctx, board.AckRequest{Actor: board.Actor{SessionAddress: w.b}, Post: decision.Post.ID}); board.CodeOf(err) != board.CodeNotAuthor {
		t.Fatalf("ack by a non-addressee = %v", err)
	}
	// The seat changes hands before it acknowledges; the new holder's ack
	// counts for the seat, and a second ack changes nothing.
	w.seat("relieve", "rusty-engine.reviewer", "crew-codex", w.x)
	if _, err := w.svc.Ack(w.ctx, board.AckRequest{Actor: board.Actor{SessionAddress: w.a, AsSeat: "rusty-engine.reviewer"}, Post: decision.Post.ID}); board.CodeOf(err) != board.CodeNotHolder {
		t.Fatalf("ack by the relieved holder = %v", err)
	}
	d, err := w.svc.Ack(w.ctx, board.AckRequest{Actor: board.Actor{SessionAddress: w.x, AsSeat: "rusty-engine.reviewer"}, Post: decision.Post.ID})
	if err != nil || len(d.Acked) != 1 || len(d.Waiting) != 1 || d.Waiting[0].Address != "lore" {
		t.Fatalf("ack by the new holder = %+v, %v", d, err)
	}
	if d, _ = w.svc.Ack(w.ctx, board.AckRequest{Actor: board.Actor{SessionAddress: w.x, AsSeat: "rusty-engine.reviewer"}, Post: decision.Post.ID}); len(d.Acked) != 1 {
		t.Fatalf("second ack = %+v", d)
	}
	if _, err := w.svc.Ack(w.ctx, board.AckRequest{Actor: board.Actor{Person: "lore"}, Post: decision.Post.ID}); err != nil {
		t.Fatal(err)
	}
	if waiting, _ = w.svc.OpenAcks(w.ctx, "", 10); len(waiting) != 0 {
		t.Fatalf("open acks after everyone acknowledged = %+v", waiting)
	}
	if _, err := w.svc.Ack(w.ctx, board.AckRequest{Actor: board.Actor{Person: "lore"}, Post: opened.Post.ID}); board.CodeOf(err) != board.CodeInvalid {
		t.Fatalf("ack of a non-decision = %v", err)
	}
}

func TestDigestIsBoundedAndDeterministic(t *testing.T) {
	w := newWorld(t)
	w.newBoard("rusty-engine")
	w.newBoard("elsewhere")
	post := func(thread, kind, body string) string {
		w.t.Helper()
		w.tick()
		out, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Thread: thread, Kind: kind, Body: body})
		if err != nil {
			t.Fatal(err)
		}
		return out.Post.ID
	}
	t1 := w.thread(board.Actor{Person: "patch"}, "Questions", "start").Thread.ID
	answered := post(t1, "question", "Which allocator?")
	w.tick()
	if _, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: board.Actor{Person: "lore"}, OperationID: w.op(), Thread: t1, Parent: answered, Kind: "answer", Body: "The arena one."}); err != nil {
		t.Fatal(err)
	}
	var open []string
	for i := 0; i < 12; i++ {
		open = append(open, post(t1, "question", "Open question "+strings.Repeat("?", i+1)))
	}
	warning := post(t1, "warning", "The cache dir fills the disk.")
	decision := post(t1, "decision", "Arena allocator it is. @lore")
	post(t1, "mystery", "an unknown kind is kept and ignored")
	w.threadChange(w.svc.Settle, board.ThreadRequest{Actor: board.Actor{Person: "patch"}, Thread: t1, Into: "wiki:allocators"})
	other := w.thread(board.Actor{Person: "patch"}, "Elsewhere", "x")
	_ = other
	if _, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: w.op(), Board: "elsewhere", Title: "Not this board", Kind: "question", Body: "Not in the digest"}); err != nil {
		t.Fatal(err)
	}

	d, err := w.svc.Digest(w.ctx, "rusty-engine")
	if err != nil {
		t.Fatal(err)
	}
	if len(d.OpenQuestions) != 10 || d.OpenQuestions[0].ID != open[11] || d.OpenQuestions[9].ID != open[2] {
		t.Fatalf("open questions: %d, first %s", len(d.OpenQuestions), d.OpenQuestions[0].ID)
	}
	for _, q := range d.OpenQuestions {
		if q.ID == answered {
			t.Fatal("an answered question is listed as open")
		}
	}
	if len(d.Warnings) != 1 || d.Warnings[0].ID != warning {
		t.Fatalf("warnings = %+v", d.Warnings)
	}
	if len(d.Decisions) != 1 || d.Decisions[0].Post.ID != decision || len(d.Decisions[0].Waiting) != 1 {
		t.Fatalf("decisions = %+v", d.Decisions)
	}
	if len(d.Settled) != 1 || d.Settled[0].ID != t1 {
		t.Fatalf("settled = %+v", d.Settled)
	}
	again, _ := w.svc.Digest(w.ctx, "rusty-engine")
	if digestKey(again) != digestKey(d) {
		t.Fatal("the digest changed between two reads")
	}
}

func digestKey(d board.Digest) string {
	var parts []string
	for _, p := range d.OpenQuestions {
		parts = append(parts, p.ID)
	}
	for _, x := range d.Decisions {
		parts = append(parts, x.Post.ID)
	}
	for _, p := range d.Warnings {
		parts = append(parts, p.ID)
	}
	for _, t := range d.Settled {
		parts = append(parts, t.ID)
	}
	return strings.Join(parts, ",")
}
