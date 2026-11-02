package board

import (
	"context"
	"regexp"
	"slices"
	"strings"
	"time"
)

func normalKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == KindNote {
		return ""
	}
	return kind
}

// linkRef is what a thread can settle into.
var linkRef = regexp.MustCompile(`^(wiki:[a-z0-9][a-z0-9/._-]*|den:[a-z0-9][a-z0-9._-]*/[0-9]+|url:https?://\S+)$`)

// ThreadRequest changes a thread as a whole: settles it, renames it or closes
// it. Any participant may; these are curation, and writes are never blocked
// by them.
type ThreadRequest struct {
	Actor
	OperationID string `json:"operation_id"`
	Thread      string `json:"thread"`
	Into        string `json:"into,omitempty"`
	Title       string `json:"title,omitempty"`
	Redirect    string `json:"redirect,omitempty"`
	Note        string `json:"note,omitempty"`
}

func (s *Service) threadChange(ctx context.Context, op string, request ThreadRequest, change func(now time.Time, who Author, t *Thread) error) (Thread, bool, error) {
	return do(s, ctx, request.OperationID, struct {
		Op string
		ThreadRequest
	}{op, request}, func(now time.Time) (Thread, []Change, error) {
		thread, err := s.thread(ctx, request.Thread)
		if err != nil {
			return Thread{}, nil, err
		}
		who, err := s.author(ctx, request.Actor)
		if err != nil {
			return Thread{}, nil, err
		}
		if err := change(now, who, &thread); err != nil {
			return Thread{}, nil, err
		}
		event := Event{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, Type: op, Note: strings.TrimSpace(request.Note), Actor: who, CreatedAt: now,
			Data: map[string]any{"into": request.Into, "title": request.Title, "redirect": request.Redirect}}
		update := ThreadUpdate{ID: thread.ID, State: thread.State, Title: thread.Title, Links: thread.Links, Aliases: thread.Aliases, RedirectTo: thread.RedirectTo}
		return thread, []Change{{ThreadUpdate: &update}, {Event: &event}}, nil
	})
}

// Settle records an artifact the thread settled into and marks it settled.
// Later posts are still accepted; a later settle adds a link.
func (s *Service) Settle(ctx context.Context, request ThreadRequest) (Thread, bool, error) {
	return s.threadChange(ctx, "settle", request, func(now time.Time, who Author, t *Thread) error {
		into := strings.TrimSpace(request.Into)
		if !linkRef.MatchString(into) {
			return refuse(CodeInvalid, "settle into wiki:SLUG, den:PROJECT/TASK or url:URL, not %q", into)
		}
		if !slices.ContainsFunc(t.Links, func(l Link) bool { return l.Ref == into }) {
			t.Links = append(t.Links, Link{Ref: into, Note: strings.TrimSpace(request.Note), By: who, At: now})
		}
		t.State = ThreadSettled
		return nil
	})
}

// Rename gives a thread a new title and keeps the old one as an alias.
func (s *Service) Rename(ctx context.Context, request ThreadRequest) (Thread, bool, error) {
	return s.threadChange(ctx, "rename", request, func(now time.Time, who Author, t *Thread) error {
		title := strings.TrimSpace(request.Title)
		if title == "" || len(title) > maxTitle {
			return refuse(CodeInvalid, "a title of 1 to %d characters is required", maxTitle)
		}
		if title != t.Title {
			if !slices.Contains(t.Aliases, t.Title) {
				t.Aliases = append(t.Aliases, t.Title)
			}
			t.Title = title
		}
		return nil
	})
}

// Close closes a thread, optionally pointing readers at another. Posting to
// a closed thread is still allowed.
func (s *Service) Close(ctx context.Context, request ThreadRequest) (Thread, bool, error) {
	return s.threadChange(ctx, "close", request, func(now time.Time, who Author, t *Thread) error {
		if request.Redirect != "" {
			target, err := s.thread(ctx, request.Redirect)
			if err != nil {
				return err
			}
			if target.ID == t.ID {
				return refuse(CodeInvalid, "a thread cannot redirect to itself")
			}
		}
		t.State, t.RedirectTo = ThreadClosed, request.Redirect
		return nil
	})
}

// AckRequest acknowledges a decision as one of its addressees.
type AckRequest struct {
	Actor
	Post string `json:"post"`
}

// Decision is a decision post and how its acknowledgements stand. Its
// addressees are the participants it mentions; for a seat, an ack by any
// holder counts.
type Decision struct {
	Post       Post          `json:"post"`
	Thread     Thread        `json:"thread"`
	Addressees []Participant `json:"addressees"`
	Acked      []Participant `json:"acked"`
	Waiting    []Participant `json:"waiting"`
}

// Ack records the caller's acknowledgement of a decision it is addressed by.
// Acknowledging again changes nothing. It sends no notice.
func (s *Service) Ack(ctx context.Context, request AckRequest) (Decision, error) {
	who, err := s.author(ctx, request.Actor)
	if err != nil {
		return Decision{}, err
	}
	post, err := s.post(ctx, request.Post)
	if err != nil {
		return Decision{}, err
	}
	if post.Kind != KindDecision || post.State != PostVisible {
		return Decision{}, refuse(CodeInvalid, "post %s is not a decision to acknowledge", post.ID)
	}
	me := who.Participant()
	if !slices.ContainsFunc(post.Mentions, func(m Mention) bool { return m.Participant() == me }) {
		return Decision{}, refuse(CodeNotAuthor, "the decision does not name %s; only the participants it mentions acknowledge it", me.Address)
	}
	thread, err := s.thread(ctx, post.ThreadID)
	if err != nil {
		return Decision{}, err
	}
	acked, err := s.store.Acks(ctx, post.ID)
	if err != nil {
		return Decision{}, err
	}
	if !slices.Contains(acked, me) {
		now := s.now().UTC()
		changes := append([]Change{{Ack: &AckChange{PostID: post.ID, For: me, By: who, At: now}}}, s.shareAck(ctx, post, thread, who, now)...)
		s.mu.Lock()
		err = s.store.Apply(ctx, Receipt{}, changes)
		s.mu.Unlock()
		if err != nil {
			return Decision{}, err
		}
	}
	return s.decision(ctx, post, thread)
}

func (s *Service) decision(ctx context.Context, post Post, thread Thread) (Decision, error) {
	acked, err := s.store.Acks(ctx, post.ID)
	if err != nil {
		return Decision{}, err
	}
	d := Decision{Post: post, Thread: thread, Acked: acked, Addressees: []Participant{}, Waiting: []Participant{}}
	for _, m := range post.Mentions {
		p := m.Participant()
		d.Addressees = append(d.Addressees, p)
		if !slices.Contains(acked, p) {
			d.Waiting = append(d.Waiting, p)
		}
	}
	return d, nil
}

// OpenAcks lists decisions still waiting on an addressee, newest first, on
// one board ("" for all).
func (s *Service) OpenAcks(ctx context.Context, board string, limit int) ([]Decision, error) {
	if board != "" {
		if _, err := s.GetBoard(ctx, board); err != nil {
			return nil, err
		}
	}
	// Decisions are read a page at a time, newest first, until enough are
	// still waiting or there are no more, so an old open decision is found
	// however many settled ones came after it.
	want, before := pageSize(limit), ""
	out := []Decision{}
	for {
		posts, err := s.store.KindPosts(ctx, board, KindDecision, before, maxPage)
		if err != nil {
			return nil, err
		}
		for _, p := range posts {
			thread, err := s.thread(ctx, p.ThreadID)
			if err != nil {
				continue
			}
			d, err := s.decision(ctx, p, thread)
			if err != nil {
				return nil, err
			}
			if len(d.Waiting) > 0 {
				out = append(out, d)
				if len(out) == want {
					return out, nil
				}
			}
		}
		if len(posts) < maxPage {
			return out, nil
		}
		before = posts[len(posts)-1].ID
	}
}

// Digest is a bounded, deterministic summary of one board; no model is
// involved.
type Digest struct {
	Board         string     `json:"board"`
	OpenQuestions []Post     `json:"open_questions"`
	Decisions     []Decision `json:"decisions"`
	Warnings      []Post     `json:"warnings"`
	Settled       []Thread   `json:"settled"`
}

const digestEach = 10

func (s *Service) Digest(ctx context.Context, board string) (Digest, error) {
	if _, err := s.GetBoard(ctx, board); err != nil {
		return Digest{}, err
	}
	d := Digest{Board: board, Decisions: []Decision{}, Settled: []Thread{}}
	var err error
	if d.OpenQuestions, err = s.store.OpenQuestions(ctx, board, digestEach); err != nil {
		return Digest{}, err
	}
	if d.Warnings, err = s.store.KindPosts(ctx, board, KindWarning, "", digestEach); err != nil {
		return Digest{}, err
	}
	decisions, err := s.store.KindPosts(ctx, board, KindDecision, "", digestEach)
	if err != nil {
		return Digest{}, err
	}
	for _, p := range decisions {
		thread, err := s.thread(ctx, p.ThreadID)
		if err != nil {
			continue
		}
		dec, err := s.decision(ctx, p, thread)
		if err != nil {
			return Digest{}, err
		}
		d.Decisions = append(d.Decisions, dec)
	}
	settled, err := s.store.ListThreads(ctx, board, ThreadCursor{}, digestEach, ThreadFilter{States: []string{ThreadSettled}})
	if err != nil {
		return Digest{}, err
	}
	d.Settled = settled
	return d, nil
}
