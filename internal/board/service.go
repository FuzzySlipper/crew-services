package board

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"
)

// Store keeps crew-board's records. Apply makes a set of changes and the
// operation's receipt in one transaction; reads never change anything.
type Store interface {
	Receipt(ctx context.Context, operationID string) (Receipt, error)
	Apply(ctx context.Context, receipt Receipt, changes []Change) error
	GetBoard(ctx context.Context, name string) (Board, error)
	ListBoards(ctx context.Context) ([]Board, error)
	GetThread(ctx context.Context, id string) (Thread, error)
	// ListThreads lists a board's threads, most recently active first,
	// starting after the cursor position (zero for the first page).
	ListThreads(ctx context.Context, board string, after ThreadCursor, limit int, filter ThreadFilter) ([]Thread, error)
	KindPosts(ctx context.Context, board, kind, beforeID string, limit int) ([]Post, error)
	OpenQuestions(ctx context.Context, board string, limit int) ([]Post, error)
	Acks(ctx context.Context, postID string) ([]Participant, error)
	GetPost(ctx context.Context, id string) (Post, error)
	// ListPosts lists a thread's posts in order, after the post afterID ("" for
	// the first page).
	ListPosts(ctx context.Context, threadID, afterID string, limit int) ([]Post, error)
	ListRevisions(ctx context.Context, postID string) ([]Revision, error)
	// Search finds visible posts on a board whose current text or thread title
	// contains query, newest first.
	Search(ctx context.Context, board, query string, limit int) ([]Post, error)

	// Subscribers lists who follows a thread, directly or through its board.
	Subscribers(ctx context.Context, threadID, board string) ([]Participant, error)
	Subscribe(ctx context.Context, sub Subscription) error
	Unsubscribe(ctx context.Context, sub Subscription) error
	Subscriptions(ctx context.Context, p Participant) ([]Subscription, error)
	// MarkRead moves a read cursor forward, never back.
	MarkRead(ctx context.Context, p Participant, threadID, postID string, at time.Time) error
	Cursor(ctx context.Context, p Participant, threadID string) (string, error)
	Unread(ctx context.Context, p Participant, limit int) ([]Unread, error)
	DueNotices(ctx context.Context, now time.Time, limit int) ([]Notice, error)
	ListNotices(ctx context.Context, state string, limit int) ([]Notice, error)
	UpdateNotice(ctx context.Context, n Notice) error
}

// Receipt is a stored write: retries with the same operation ID and request
// replay Result.
type Receipt struct {
	OperationID string
	RequestHash string
	Result      json.RawMessage
	CreatedAt   time.Time
	// Purged marks a receipt whose result a purge emptied.
	Purged bool
}

// ThreadCursor is a position in a board's thread listing.
type ThreadCursor struct {
	LastActivityAt time.Time
	ID             string
}

// ThreadFilter narrows a thread listing: by state, and to threads active
// since a time (zero for all).
type ThreadFilter struct {
	States      []string
	ActiveSince time.Time
}

// ThreadUpdate sets a thread's state, title, links, old titles and redirect.
type ThreadUpdate struct {
	ID, State, Title, RedirectTo string
	Links                        []Link
	Aliases                      []string
}

// AckChange records that For acknowledged a decision; By is who did it (for a
// seat, the session holding it).
type AckChange struct {
	PostID string
	For    Participant
	By     Author
	At     time.Time
}

// Change is one row a write adds or updates. Exactly one field is set.
type Change struct {
	Outbox       *OutboxFile
	ThreadUpdate *ThreadUpdate
	Ack          *AckChange
	Notice       *Notice
	Subscribe    *Subscription
	Board        *Board
	Thread       *Thread
	Post         *Post
	Revision     *Revision
	Event        *Event
	Touch        *Touch
	PostState    *PostState
	// ScrubPost removes a post's text from every revision and its author.
	ScrubPost string
	// ScrubThread does that for every post of a thread and its title.
	ScrubThread string
}

// Touch records new activity in a thread.
type Touch struct {
	ThreadID string
	At       time.Time
}

// PostState sets a post's state, current revision and note.
type PostState struct {
	PostID   string
	State    string
	Revision int
	Note     string
	EditedAt time.Time
}

// Directory answers who a caller is, from the crew messaging fabric.
type Directory interface {
	// Session reports whether address is a bound session address and which
	// adapter drives it.
	Session(ctx context.Context, address string) (adapter string, bound bool, err error)
	// SeatHolder returns the session holding seat, "" when it is vacant, and
	// exists false when there is no such seat or it is retired.
	SeatHolder(ctx context.Context, seat string) (holder string, exists bool, err error)
}

// Config is crew-board's typed configuration.
type Config struct {
	// Site is this site's name, the prefix of every ID it makes.
	Site string
	// People are the people on this site, by short name. crew-humans will own
	// this list (#10535); until then it is configured here.
	People []string
	// NoticeRetry is how long a notice is retried while the fabric cannot be
	// reached before it is marked failed.
	NoticeRetry time.Duration
	// ArchiveAfter is how long a thread may go without activity before it
	// drops out of the default listing. It is never purged for that.
	ArchiveAfter time.Duration
	// Networks are the networks this site shares boards on.
	Networks []NetworkInfo
}

const (
	defaultPage = 50
	maxPage     = 200
	maxBody     = 1 << 20
	maxTitle    = 200
)

// Service is crew-board's rules. Writes are serialized; reads run freely.
type Service struct {
	store Store
	dir   Directory
	now   func() time.Time
	cfg   Config
	mu    sync.Mutex
	poke  func()
	// sharePoke asks the network syncers to push now.
	sharePoke func()

	media       *Ingester
	attachments AttachmentStore
}

var siteName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func New(store Store, dir Directory, now func() time.Time, cfg Config) (*Service, error) {
	if store == nil || dir == nil || now == nil {
		return nil, errors.New("board: store, directory and clock are required")
	}
	if !siteName.MatchString(cfg.Site) {
		return nil, errors.New("board: site must be lowercase letters, digits and hyphens")
	}
	for _, p := range cfg.People {
		if !personName.MatchString(p) {
			return nil, errors.New("board: person " + p + " must be lowercase letters, digits and hyphens")
		}
	}
	if cfg.NoticeRetry <= 0 {
		cfg.NoticeRetry = time.Hour
	}
	if cfg.ArchiveAfter <= 0 {
		cfg.ArchiveAfter = 30 * 24 * time.Hour
	}
	return &Service{store: store, dir: dir, now: now, cfg: cfg, poke: func() {}, sharePoke: func() {}}, nil
}

// OnShare sets what to call after a write queues files for a network.
func (s *Service) OnShare(poke func()) { s.sharePoke = poke }

// OnNotices sets what to call after a write queues notices, so the sender
// can go at once instead of on its next tick.
func (s *Service) OnNotices(poke func()) { s.poke = poke }

// Site is this site's name.
func (s *Service) Site() string { return s.cfg.Site }

// People lists the people on this site.
func (s *Service) People() []string { return slices.Clone(s.cfg.People) }

var (
	boardName  = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{0,63}$`)
	personName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)
	seatForm   = regexp.MustCompile(`^[a-z0-9-]+\.[a-z0-9-]+$`)
)

// author resolves who the actor is, checked against the fabric: a person on
// this site, a bound session, or a session posting as a seat it holds.
func (s *Service) author(ctx context.Context, actor Actor) (Author, error) {
	actor.SessionAddress, actor.AsSeat, actor.Person = strings.TrimSpace(actor.SessionAddress), strings.TrimSpace(actor.AsSeat), strings.TrimSpace(actor.Person)
	if actor.Person != "" {
		if actor.SessionAddress != "" || actor.AsSeat != "" {
			return Author{}, refuse(CodeInvalid, "a person writes outside any session; give person or session_address, not both")
		}
		if !slices.Contains(s.cfg.People, actor.Person) {
			return Author{}, refuse(CodeUnknownPerson, "%s is not a person on this site", actor.Person)
		}
		return Author{Kind: AuthorPerson, Address: actor.Person, Site: s.cfg.Site}, nil
	}
	if !strings.HasPrefix(actor.SessionAddress, "s/") {
		return Author{}, refuse(CodeInvalid, "session_address (s/…) or person is required")
	}
	adapter, bound, err := s.dir.Session(ctx, actor.SessionAddress)
	if err != nil {
		return Author{}, &Error{Code: CodeDirectoryUnreached, Err: err}
	}
	if !bound {
		return Author{}, refuse(CodeSessionUnbound, "%s is not a bound session address", actor.SessionAddress)
	}
	if actor.AsSeat == "" {
		return Author{Kind: AuthorSession, Address: actor.SessionAddress, Adapter: adapter, Model: cleanModel(actor.Model), Site: s.cfg.Site}, nil
	}
	if !seatForm.MatchString(actor.AsSeat) {
		return Author{}, refuse(CodeInvalid, "as_seat must be a seat address, scope.name")
	}
	holder, exists, err := s.dir.SeatHolder(ctx, actor.AsSeat)
	if err != nil {
		return Author{}, &Error{Code: CodeDirectoryUnreached, Err: err}
	}
	if !exists || holder != actor.SessionAddress {
		return Author{}, &Error{Code: CodeNotHolder, Err: errors.New(actor.SessionAddress + " does not hold " + actor.AsSeat), Detail: map[string]any{"holder": holder}}
	}
	return Author{Kind: AuthorSeat, Address: actor.AsSeat, Session: actor.SessionAddress, Adapter: adapter, Model: cleanModel(actor.Model), Site: s.cfg.Site}, nil
}

// sameParticipant reports whether the caller is the post's author: the same
// person, the same session, or (for a seat) whoever holds that seat now.
func sameParticipant(caller, author Author) bool {
	return caller.Kind == author.Kind && caller.Address == author.Address && caller.Site == author.Site
}

// resolveMentions resolves a body's references on this site. A reference that
// names nothing here stays plain text and comes back as a warning.
func (s *Service) resolveMentions(ctx context.Context, body string) ([]Mention, []MentionWarning) {
	var mentions []Mention
	var warnings []MentionWarning
	for _, c := range mentionCandidates(body) {
		if c.Site != "" && c.Site != s.cfg.Site {
			// Another site vouches for its own participants; this site cannot
			// check them, only that it shares a network with that site.
			if !s.partnerSite(c.Site) {
				warnings = append(warnings, MentionWarning{Text: c.Text, Reason: "this site shares no board with " + c.Site})
				continue
			}
			mentions = append(mentions, Mention{Text: c.Text, Kind: participantKind(c.Name), Address: c.Name, Site: c.Site})
			continue
		}
		kind := participantKind(c.Name)
		reason := ""
		switch kind {
		case AuthorSession:
			if _, bound, err := s.dir.Session(ctx, c.Name); err != nil {
				reason = "could not check it: " + err.Error()
			} else if !bound {
				reason = "no bound session has that address"
			}
		case AuthorSeat:
			if _, exists, err := s.dir.SeatHolder(ctx, c.Name); err != nil {
				reason = "could not check it: " + err.Error()
			} else if !exists {
				reason = "no seat by that name (or it is retired)"
			}
		case AuthorPerson:
			if !slices.Contains(s.cfg.People, c.Name) {
				reason = "names no seat, session or person on this site; to reach an agent, mention its seat or session"
			}
		}
		if reason != "" {
			warnings = append(warnings, MentionWarning{Text: c.Text, Reason: reason})
			continue
		}
		mentions = append(mentions, Mention{Text: c.Text, Kind: kind, Address: c.Name, Site: s.cfg.Site})
	}
	return mentions, warnings
}

// do runs one write under the operation's receipt: an exact retry replays the
// stored result, and the same ID with a different request is refused.
func do[T any](s *Service, ctx context.Context, operationID string, request any, run func(now time.Time) (T, []Change, error)) (T, bool, error) {
	var zero T
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return zero, false, refuse(CodeInvalid, "operation_id is required")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return zero, false, err
	}
	sum := sha256.Sum256(encoded)
	hash := hex.EncodeToString(sum[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	if receipt, err := s.store.Receipt(ctx, operationID); err == nil {
		if receipt.RequestHash != hash {
			return zero, false, refuse(CodeOperationConflict, "operation %s was used for a different request", operationID)
		}
		if receipt.Purged {
			return zero, false, refuse(CodePostNotFound, "what operation %s wrote has been purged", operationID)
		}
		var result T
		if err := json.Unmarshal(receipt.Result, &result); err != nil {
			return zero, false, err
		}
		return result, true, nil
	} else if !errors.Is(err, ErrNotFound) {
		return zero, false, err
	}
	now := s.now().UTC()
	result, changes, err := run(now)
	if err != nil {
		return zero, false, err
	}
	if changes, err = s.share(ctx, changes); err != nil {
		return zero, false, err
	}
	stored, err := json.Marshal(result)
	if err != nil {
		return zero, false, err
	}
	if err := s.store.Apply(ctx, Receipt{OperationID: operationID, RequestHash: hash, Result: stored, CreatedAt: now}, changes); err != nil {
		return zero, false, err
	}
	for _, c := range changes {
		if c.Notice != nil {
			s.poke()
			break
		}
	}
	for _, c := range changes {
		if c.Outbox != nil {
			s.sharePoke()
			break
		}
	}
	return result, false, nil
}

// CreateBoardRequest makes a local board.
type CreateBoardRequest struct {
	Actor
	OperationID string `json:"operation_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Network shares the board on a network from its creation.
	Network string `json:"network,omitempty"`
}

func (s *Service) CreateBoard(ctx context.Context, request CreateBoardRequest) (Board, bool, error) {
	return do(s, ctx, request.OperationID, request, func(now time.Time) (Board, []Change, error) {
		name := strings.TrimSpace(request.Name)
		if !boardName.MatchString(name) {
			return Board{}, nil, refuse(CodeInvalidBoardName, "board names are lowercase letters, digits, '-' and '.': %q", name)
		}
		author, err := s.author(ctx, request.Actor)
		if err != nil {
			return Board{}, nil, err
		}
		if _, err := s.store.GetBoard(ctx, name); err == nil {
			return Board{}, nil, refuse(CodeBoardExists, "board %s already exists", name)
		} else if !errors.Is(err, ErrNotFound) {
			return Board{}, nil, err
		}
		if request.Network != "" {
			if _, ok := s.network(request.Network); !ok {
				return Board{}, nil, refuse(CodeInvalid, "no network %s is configured on this site", request.Network)
			}
		}
		board := Board{Name: name, Site: s.cfg.Site, Description: strings.TrimSpace(request.Description), CreatedAt: now, CreatedBy: author, Network: request.Network}
		return board, []Change{{Board: &board}}, nil
	})
}

// PostResult is a written post, with the mentions that stayed plain text.
type PostResult struct {
	Thread   Thread           `json:"thread"`
	Post     Post             `json:"post"`
	Warnings []MentionWarning `json:"warnings,omitempty"`
}

// OpenThreadRequest opens a thread with its first post.
type OpenThreadRequest struct {
	Actor
	OperationID string `json:"operation_id"`
	Board       string `json:"board"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	Urgent      bool   `json:"urgent,omitempty"`
	Kind        string `json:"kind,omitempty"`
	// NoSubscribe opts out of following the thread just opened.
	NoSubscribe bool `json:"no_subscribe,omitempty"`
}

func (s *Service) OpenThread(ctx context.Context, request OpenThreadRequest) (PostResult, bool, error) {
	return do(s, ctx, request.OperationID, request, func(now time.Time) (PostResult, []Change, error) {
		title := strings.TrimSpace(request.Title)
		if title == "" || len(title) > maxTitle {
			return PostResult{}, nil, refuse(CodeInvalid, "a title of 1 to %d characters is required", maxTitle)
		}
		if err := checkBody(request.Body); err != nil {
			return PostResult{}, nil, err
		}
		if err := s.checkAttachments(ctx, request.Body); err != nil {
			return PostResult{}, nil, err
		}
		if _, err := s.store.GetBoard(ctx, request.Board); errors.Is(err, ErrNotFound) {
			return PostResult{}, nil, refuse(CodeBoardNotFound, "no board %s", request.Board)
		} else if err != nil {
			return PostResult{}, nil, err
		}
		author, err := s.author(ctx, request.Actor)
		if err != nil {
			return PostResult{}, nil, err
		}
		mentions, warnings := s.resolveMentions(ctx, request.Body)
		thread := Thread{ID: NewID(s.cfg.Site, now), Board: request.Board, Title: title, State: ThreadOpen, CreatedAt: now, CreatedBy: author, LastActivityAt: now, Posts: 1}
		post := Post{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, Author: author, Site: s.cfg.Site, State: PostVisible, Revision: 1, Body: request.Body, Mentions: mentions, Urgent: request.Urgent, Kind: normalKind(request.Kind), CreatedAt: now}
		thread.OpeningPostID = post.ID
		revision := Revision{PostID: post.ID, Revision: 1, Body: request.Body, Mentions: mentions, Model: author.Model, CreatedAt: now}
		changes := []Change{{Thread: &thread}, {Post: &post}, {Revision: &revision}}
		notices, err := s.noticesFor(ctx, now, thread, post, "", nil)
		if err != nil {
			return PostResult{}, nil, err
		}
		changes = append(changes, notices...)
		if !request.NoSubscribe {
			changes = append(changes, Change{Subscribe: &Subscription{Participant: author.Participant(), Thread: thread.ID, CreatedAt: now}})
		}
		return PostResult{Thread: thread, Post: post, Warnings: warnings}, changes, nil
	})
}

// ReplyRequest adds a post to a thread. Parent is the post it answers; empty
// means the thread's opening post.
type ReplyRequest struct {
	Actor
	OperationID string `json:"operation_id"`
	Thread      string `json:"thread"`
	Parent      string `json:"parent,omitempty"`
	Body        string `json:"body"`
	Urgent      bool   `json:"urgent,omitempty"`
	Kind        string `json:"kind,omitempty"`
}

func (s *Service) Reply(ctx context.Context, request ReplyRequest) (PostResult, bool, error) {
	return do(s, ctx, request.OperationID, request, func(now time.Time) (PostResult, []Change, error) {
		if err := checkBody(request.Body); err != nil {
			return PostResult{}, nil, err
		}
		if err := s.checkAttachments(ctx, request.Body); err != nil {
			return PostResult{}, nil, err
		}
		thread, err := s.thread(ctx, request.Thread)
		if err != nil {
			return PostResult{}, nil, err
		}
		parent := thread.OpeningPostID
		var parentAuthor *Author
		if request.Parent != "" {
			p, err := s.store.GetPost(ctx, request.Parent)
			if errors.Is(err, ErrNotFound) {
				return PostResult{}, nil, refuse(CodePostNotFound, "no post %s", request.Parent)
			} else if err != nil {
				return PostResult{}, nil, err
			}
			if p.ThreadID != thread.ID {
				return PostResult{}, nil, refuse(CodeParentNotInThread, "post %s is in another thread", request.Parent)
			}
			parent = p.ID
			if p.State == PostVisible {
				parentAuthor = &p.Author
			}
		} else if p, err := s.store.GetPost(ctx, parent); err == nil && p.State == PostVisible {
			parentAuthor = &p.Author
		}
		author, err := s.author(ctx, request.Actor)
		if err != nil {
			return PostResult{}, nil, err
		}
		mentions, warnings := s.resolveMentions(ctx, request.Body)
		post := Post{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, ParentID: parent, Author: author, Site: s.cfg.Site, State: PostVisible, Revision: 1, Body: request.Body, Mentions: mentions, Urgent: request.Urgent, Kind: normalKind(request.Kind), CreatedAt: now}
		revision := Revision{PostID: post.ID, Revision: 1, Body: request.Body, Mentions: mentions, Model: author.Model, CreatedAt: now}
		thread.LastActivityAt, thread.Posts = now, thread.Posts+1
		changes := []Change{{Post: &post}, {Revision: &revision}, {Touch: &Touch{ThreadID: thread.ID, At: now}}}
		var replyTo []Participant
		if parentAuthor != nil {
			replyTo = []Participant{parentAuthor.Participant()}
		}
		notices, err := s.noticesFor(ctx, now, thread, post, ReasonReply, replyTo)
		if err != nil {
			return PostResult{}, nil, err
		}
		return PostResult{Thread: thread, Post: post, Warnings: warnings}, append(changes, notices...), nil
	})
}

// EditRequest replaces a post's text with a new revision. Only its author
// may edit it; for a seat, that is whoever holds the seat now.
type EditRequest struct {
	Actor
	OperationID string `json:"operation_id"`
	Post        string `json:"post"`
	Body        string `json:"body"`
}

func (s *Service) Edit(ctx context.Context, request EditRequest) (PostResult, bool, error) {
	return do(s, ctx, request.OperationID, request, func(now time.Time) (PostResult, []Change, error) {
		if err := checkBody(request.Body); err != nil {
			return PostResult{}, nil, err
		}
		if err := s.checkAttachments(ctx, request.Body); err != nil {
			return PostResult{}, nil, err
		}
		post, thread, err := s.authorsPost(ctx, request.Actor, request.Post)
		if err != nil {
			return PostResult{}, nil, err
		}
		if post.State != PostVisible {
			return PostResult{}, nil, refuse(CodePostNotVisible, "post %s is %s", post.ID, post.State)
		}
		mentions, warnings := s.resolveMentions(ctx, request.Body)
		earlier := map[Participant]bool{}
		for _, m := range post.Mentions {
			earlier[m.Participant()] = true
		}
		revision := Revision{PostID: post.ID, Revision: post.Revision + 1, Body: request.Body, Mentions: mentions, Model: editorModel(request.Actor), CreatedAt: now}
		post.Revision, post.Body, post.Mentions, post.EditedAt = revision.Revision, request.Body, mentions, now
		thread.LastActivityAt = now
		changes := []Change{
			{Revision: &revision},
			{PostState: &PostState{PostID: post.ID, State: PostVisible, Revision: revision.Revision, EditedAt: now}},
			{Touch: &Touch{ThreadID: thread.ID, At: now}},
		}
		// An edit tells only the participants it newly mentions.
		var added []Mention
		for _, m := range mentions {
			if !earlier[m.Participant()] {
				added = append(added, m)
			}
		}
		notices, err := s.mentionNotices(ctx, now, thread, post, added)
		if err != nil {
			return PostResult{}, nil, err
		}
		return PostResult{Thread: thread, Post: post, Warnings: warnings}, append(changes, notices...), nil
	})
}

// ModerateRequest retracts, hides or purges a post. Retracting is for the
// post's author; hiding and purging are for a person on this site.
type ModerateRequest struct {
	Actor
	OperationID string `json:"operation_id"`
	Post        string `json:"post"`
	Note        string `json:"note,omitempty"`
}

// Retract withdraws the caller's own post. Its text is no longer shown, and a
// tombstone keeps the thread navigable.
func (s *Service) Retract(ctx context.Context, request ModerateRequest) (PostResult, bool, error) {
	return do(s, ctx, request.OperationID, struct {
		Op string
		ModerateRequest
	}{"retract", request}, func(now time.Time) (PostResult, []Change, error) {
		post, thread, err := s.authorsPost(ctx, request.Actor, request.Post)
		if err != nil {
			return PostResult{}, nil, err
		}
		if post.State != PostVisible {
			return PostResult{}, nil, refuse(CodePostNotVisible, "post %s is %s", post.ID, post.State)
		}
		note := strings.TrimSpace(request.Note)
		post.State, post.Note = PostRetracted, note
		event := Event{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, PostID: post.ID, Type: "retract", Note: note, Actor: post.Author, CreatedAt: now}
		return PostResult{Thread: thread, Post: public(post)}, []Change{{PostState: &PostState{PostID: post.ID, State: PostRetracted, Revision: post.Revision, Note: note, EditedAt: post.EditedAt}}, {Event: &event}}, nil
	})
}

// Hide takes a post out of this site's normal reads, as moderation.
func (s *Service) Hide(ctx context.Context, request ModerateRequest) (PostResult, bool, error) {
	return do(s, ctx, request.OperationID, struct {
		Op string
		ModerateRequest
	}{"hide", request}, func(now time.Time) (PostResult, []Change, error) {
		person, post, thread, err := s.moderated(ctx, request)
		if err != nil {
			return PostResult{}, nil, err
		}
		if post.State != PostVisible {
			return PostResult{}, nil, refuse(CodePostNotVisible, "post %s is %s", post.ID, post.State)
		}
		post.State = PostHidden
		event := Event{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, PostID: post.ID, Type: "hide", Note: strings.TrimSpace(request.Note), Actor: person, CreatedAt: now}
		return PostResult{Thread: thread, Post: public(post)}, []Change{{PostState: &PostState{PostID: post.ID, State: PostHidden, Revision: post.Revision, Note: post.Note, EditedAt: post.EditedAt}}, {Event: &event}}, nil
	})
}

// Purge scrubs a post's text, every revision of it and its author, leaving a
// content-free tombstone. Purging a thread's opening post purges the thread.
func (s *Service) Purge(ctx context.Context, request ModerateRequest) (PostResult, bool, error) {
	var refs []string
	result, replayed, err := s.purge(ctx, request, &refs)
	if err == nil && !replayed {
		err = s.dropUnreferenced(ctx, refs)
	}
	return result, replayed, err
}

func (s *Service) purge(ctx context.Context, request ModerateRequest, refs *[]string) (PostResult, bool, error) {
	return do(s, ctx, request.OperationID, struct {
		Op string
		ModerateRequest
	}{"purge", request}, func(now time.Time) (PostResult, []Change, error) {
		person, post, thread, err := s.moderated(ctx, request)
		if err != nil {
			return PostResult{}, nil, err
		}
		// The audit event keeps who purged and when, never what was purged.
		event := Event{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, PostID: post.ID, Type: "purge", Note: strings.TrimSpace(request.Note), Actor: person, CreatedAt: now}
		if post.ID == thread.OpeningPostID {
			*refs = s.refsOf(ctx, thread.ID, "")
			return PostResult{Post: Post{ID: post.ID, ThreadID: thread.ID, State: PostPurged}}, []Change{{ScrubThread: thread.ID}, {Event: &event}}, nil
		}
		*refs = s.refsOf(ctx, thread.ID, post.ID)
		return PostResult{Thread: thread, Post: Post{ID: post.ID, ThreadID: thread.ID, ParentID: post.ParentID, State: PostPurged}}, []Change{{ScrubPost: post.ID}, {Event: &event}}, nil
	})
}

func (s *Service) moderated(ctx context.Context, request ModerateRequest) (Author, Post, Thread, error) {
	if strings.TrimSpace(request.Person) == "" {
		return Author{}, Post{}, Thread{}, refuse(CodePersonOnly, "only a person on this site can hide or purge a post")
	}
	person, err := s.author(ctx, request.Actor)
	if err != nil {
		return Author{}, Post{}, Thread{}, err
	}
	post, err := s.post(ctx, request.Post)
	if err != nil {
		return Author{}, Post{}, Thread{}, err
	}
	thread, err := s.thread(ctx, post.ThreadID)
	return person, post, thread, err
}

func (s *Service) authorsPost(ctx context.Context, actor Actor, id string) (Post, Thread, error) {
	post, err := s.post(ctx, id)
	if err != nil {
		return Post{}, Thread{}, err
	}
	caller, err := s.author(ctx, actor)
	if err != nil {
		return Post{}, Thread{}, err
	}
	if !sameParticipant(caller, post.Author) {
		return Post{}, Thread{}, refuse(CodeNotAuthor, "post %s was written by %s", post.ID, post.Author.Address)
	}
	thread, err := s.thread(ctx, post.ThreadID)
	return post, thread, err
}

func checkBody(body string) error {
	if strings.TrimSpace(body) == "" {
		return refuse(CodeInvalid, "a post needs text")
	}
	if len(body) > maxBody {
		return refuse(CodeInvalid, "a post is at most %d bytes", maxBody)
	}
	return nil
}

func (s *Service) thread(ctx context.Context, id string) (Thread, error) {
	thread, err := s.store.GetThread(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Thread{}, refuse(CodeThreadNotFound, "no thread %s", id)
	}
	return thread, err
}

func (s *Service) post(ctx context.Context, id string) (Post, error) {
	post, err := s.store.GetPost(ctx, id)
	if errors.Is(err, ErrNotFound) || err == nil && post.State == PostPurged {
		return Post{}, refuse(CodePostNotFound, "no post %s", id)
	}
	return post, err
}

// public is a post as normal reads show it: withdrawn and moderated posts keep
// their place in the thread but not their text.
func public(post Post) Post {
	switch post.State {
	case PostRetracted:
		post.Body, post.Mentions = "", nil
	case PostHidden:
		post.Body, post.Mentions, post.Note = "", nil, ""
	case PostPurged:
		post = Post{ID: post.ID, ThreadID: post.ThreadID, ParentID: post.ParentID, State: PostPurged, CreatedAt: post.CreatedAt}
	}
	return post
}

func pageSize(limit int) int {
	if limit <= 0 {
		return defaultPage
	}
	return min(limit, maxPage)
}

// ListBoards lists this site's boards.
func (s *Service) ListBoards(ctx context.Context) ([]Board, error) {
	return s.store.ListBoards(ctx)
}

func (s *Service) GetBoard(ctx context.Context, name string) (Board, error) {
	board, err := s.store.GetBoard(ctx, name)
	if errors.Is(err, ErrNotFound) {
		return Board{}, refuse(CodeBoardNotFound, "no board %s", name)
	}
	return board, err
}

// ThreadPage is one page of a board's threads; Next continues it.
type ThreadPage struct {
	Threads []Thread `json:"threads"`
	Next    string   `json:"next,omitempty"`
}

// ListThreads lists a board's threads, most recently active first. Without
// archived, threads quiet for longer than ArchiveAfter are left out; states
// narrows it to some thread states (none for all).
func (s *Service) ListThreads(ctx context.Context, board, after string, limit int, states []string, archived bool) (ThreadPage, error) {
	if _, err := s.GetBoard(ctx, board); err != nil {
		return ThreadPage{}, err
	}
	cursor, err := decodeCursor(after)
	if err != nil {
		return ThreadPage{}, err
	}
	limit = pageSize(limit)
	filter := ThreadFilter{States: states}
	if !archived {
		filter.ActiveSince = s.now().UTC().Add(-s.cfg.ArchiveAfter)
	}
	threads, err := s.store.ListThreads(ctx, board, cursor, limit+1, filter)
	if err != nil {
		return ThreadPage{}, err
	}
	page := ThreadPage{Threads: threads}
	if len(threads) > limit {
		page.Threads = threads[:limit]
		last := page.Threads[limit-1]
		page.Next = encodeCursor(ThreadCursor{LastActivityAt: last.LastActivityAt, ID: last.ID})
	}
	return page, nil
}

// ThreadRead is a thread and one page of its posts; Next continues it.
type ThreadRead struct {
	Thread Thread `json:"thread"`
	Posts  []Post `json:"posts"`
	Next   string `json:"next,omitempty"`
}

// ReadThread returns a thread and its posts in order, after the post after
// ("" from the start). Reading changes nothing.
func (s *Service) ReadThread(ctx context.Context, id, after string, limit int) (ThreadRead, error) {
	thread, err := s.thread(ctx, id)
	if err != nil {
		return ThreadRead{}, err
	}
	limit = pageSize(limit)
	posts, err := s.store.ListPosts(ctx, id, after, limit+1)
	if err != nil {
		return ThreadRead{}, err
	}
	read := ThreadRead{Thread: thread}
	if len(posts) > limit {
		posts = posts[:limit]
		read.Next = posts[limit-1].ID
	}
	for _, p := range posts {
		p.Remote = p.Site != s.cfg.Site
		read.Posts = append(read.Posts, s.withAttachments(ctx, public(p)))
	}
	return read, nil
}

// GetPost returns one post as normal reads show it.
func (s *Service) GetPost(ctx context.Context, id string) (Post, error) {
	post, err := s.store.GetPost(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Post{}, refuse(CodePostNotFound, "no post %s", id)
	}
	post.Remote = post.Site != "" && post.Site != s.cfg.Site
	return s.withAttachments(ctx, public(post)), err
}

// Revisions lists a visible post's revisions, oldest first.
func (s *Service) Revisions(ctx context.Context, id string) ([]Revision, error) {
	post, err := s.post(ctx, id)
	if err != nil {
		return nil, err
	}
	if post.State != PostVisible {
		return nil, refuse(CodePostNotVisible, "post %s is %s", id, post.State)
	}
	return s.store.ListRevisions(ctx, id)
}

// Search finds visible posts on a board by text, newest first.
func (s *Service) Search(ctx context.Context, board, query string, limit int) ([]Post, error) {
	if _, err := s.GetBoard(ctx, board); err != nil {
		return nil, err
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, refuse(CodeInvalid, "a search needs text")
	}
	return s.store.Search(ctx, board, query, pageSize(limit))
}

func encodeCursor(c ThreadCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.LastActivityAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

func decodeCursor(value string) (ThreadCursor, error) {
	if value == "" {
		return ThreadCursor{}, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err == nil {
		at, id, ok := strings.Cut(string(raw), "|")
		if parsed, perr := time.Parse(time.RFC3339Nano, at); ok && perr == nil {
			return ThreadCursor{LastActivityAt: parsed, ID: id}, nil
		}
	}
	return ThreadCursor{}, refuse(CodeInvalid, "after is not a cursor this listing returned")
}

// cleanModel keeps a reported model name only when it looks like one: short,
// with no spaces or markup. It is display information, so anything else is
// dropped rather than refused.
func cleanModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > 80 {
		return ""
	}
	for _, r := range model {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._:/@+-", r)) {
			return ""
		}
	}
	return model
}

// editorModel is the model behind an edit: a session's report, never a
// person's.
func editorModel(actor Actor) string {
	if actor.SessionAddress == "" {
		return ""
	}
	return cleanModel(actor.Model)
}
