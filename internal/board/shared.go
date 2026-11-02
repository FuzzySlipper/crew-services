package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// NetworkInfo is one network of sites this site shares boards with, as the
// board service needs it: who the partners are and how their posts may wake
// local sessions. The git side is the network package's.
type NetworkInfo struct {
	Name string
	// Partners maps a partner site to the activation its mentions and replies
	// get here: no_wake unless this site chose otherwise.
	Partners map[string]string
}

// Sync states of a local post on a shared board.
const (
	SyncPending = "pending_push"
	SyncSynced  = "synced"
)

// OutboxFile is a repo file a write on a shared board queued for its network.
type OutboxFile struct {
	ID      int64  `json:"id"`
	Network string `json:"network"`
	Path    string `json:"path"`
	Content []byte `json:"-"`
	PostID  string `json:"post_id,omitempty"`
}

// SyncState is how far a site has read its network's repo.
type SyncState struct {
	Network    string    `json:"network"`
	LastCommit string    `json:"last_commit"`
	Status     string    `json:"status"`
	CheckedAt  time.Time `json:"checked_at"`
	RepoBytes  int64     `json:"repo_bytes"`
	Pending    int       `json:"pending_files"`
}

// SyncFlag is a file a pull ignored, and why.
type SyncFlag struct {
	Network   string    `json:"network"`
	Commit    string    `json:"commit"`
	Path      string    `json:"path"`
	Reason    string    `json:"reason"`
	CreatedAt time.Time `json:"created_at"`
}

// SharedStore keeps the outbox and sync state.
type SharedStore interface {
	PendingOutbox(ctx context.Context, network string, limit int) ([]OutboxFile, error)
	// OutboxDone marks files pushed in commit and posts with nothing left to
	// push as synced.
	OutboxDone(ctx context.Context, ids []int64, commit string) error
	GetSyncState(ctx context.Context, network string) (SyncState, error)
	PutSyncState(ctx context.Context, s SyncState) error
	AddFlag(ctx context.Context, f SyncFlag) error
	ListFlags(ctx context.Context, network string, limit int) ([]SyncFlag, error)
}

func (s *Service) network(name string) (NetworkInfo, bool) {
	for _, n := range s.cfg.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return NetworkInfo{}, false
}

// partnerSite reports whether site shares a network with this one.
func (s *Service) partnerSite(site string) bool {
	for _, n := range s.cfg.Networks {
		if _, ok := n.Partners[site]; ok {
			return true
		}
	}
	return false
}

// boardNetwork is the network a board is shared on, or "".
func (s *Service) boardNetwork(ctx context.Context, board string, changes []Change) string {
	for _, c := range changes {
		if c.Board != nil && c.Board.Name == board {
			return c.Board.Network
		}
	}
	b, err := s.store.GetBoard(ctx, board)
	if err != nil {
		return ""
	}
	return b.Network
}

// share adds the repo files a write's changes mean for a shared board. The
// files go into the same transaction, so a write and its share are never
// apart.
func (s *Service) share(ctx context.Context, changes []Change) ([]Change, error) {
	threads := map[string]Thread{}
	posts := map[string]Post{}
	for _, c := range changes {
		if c.Thread != nil {
			threads[c.Thread.ID] = *c.Thread
		}
		if c.Post != nil {
			posts[c.Post.ID] = *c.Post
		}
	}
	threadOf := func(id string) (Thread, bool) {
		if t, ok := threads[id]; ok {
			return t, true
		}
		t, err := s.store.GetThread(ctx, id)
		return t, err == nil
	}
	postOf := func(id string) (Post, bool) {
		if p, ok := posts[id]; ok {
			return p, true
		}
		p, err := s.store.GetPost(ctx, id)
		return p, err == nil
	}
	var out []Change
	add := func(network, path string, content []byte, post string) {
		out = append(out, Change{Outbox: &OutboxFile{Network: network, Path: path, Content: content, PostID: post}})
	}
	for _, c := range changes {
		switch {
		case c.Board != nil && c.Board.Network != "":
			add(c.Board.Network, BoardFilePath(c.Board.Name), EncodeBoard(*c.Board), "")
		case c.Thread != nil:
			if network := s.boardNetwork(ctx, c.Thread.Board, changes); network != "" {
				add(network, ThreadFilePath(c.Thread.Board, c.Thread.ID), EncodeThread(*c.Thread), "")
			}
		case c.Revision != nil:
			p, ok := postOf(c.Revision.PostID)
			if !ok {
				continue
			}
			t, ok := threadOf(p.ThreadID)
			if !ok {
				continue
			}
			network := s.boardNetwork(ctx, t.Board, changes)
			if network == "" {
				continue
			}
			files, err := s.blobFiles(ctx, c.Revision.Body)
			if err != nil {
				return nil, err
			}
			for path, data := range files {
				add(network, path, data, "")
			}
			add(network, PostFilePath(t.Board, t.ID, p.ID, c.Revision.Revision), EncodePost(t.Board, p, *c.Revision), p.ID)
		case c.Event != nil && c.Event.Type != "hide":
			t, ok := threadOf(c.Event.ThreadID)
			if !ok {
				continue
			}
			if network := s.boardNetwork(ctx, t.Board, changes); network != "" {
				add(network, EventFilePath(t.Board, t.ID, c.Event.ID), EncodeEvent(t.Board, *c.Event, s.cfg.Site), c.Event.PostID)
			}
		}
	}
	return append(changes, out...), nil
}

// blobFiles are the repo files of the attachments a body shows. An attachment
// still held cannot be shared yet.
func (s *Service) blobFiles(ctx context.Context, body string) (map[string][]byte, error) {
	refs := attachmentRefs(body)
	if len(refs) == 0 {
		return nil, nil
	}
	out := map[string][]byte{}
	for _, ref := range refs {
		a, path, err := s.BlobPath(ctx, ref)
		if err != nil {
			if CodeOf(err) == CodeAttachmentHeld {
				return nil, refuse(CodeAttachmentHeld, "attachment %s is held; a person must release it before it can go on a shared board", short(ref))
			}
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		out[BlobFilePath(ref, filepath.Ext(path))] = data
		_ = a
	}
	return out, nil
}

// RemoteFile is one file a pull read from a network's repo, added by a commit
// signed by Signer (a pinned partner site, or this site).
type RemoteFile struct {
	Network string
	Commit  string
	Signer  string
	Path    string
	File    RepoFile
	Data    []byte
}

// Ignored is a remote file a pull does not apply, with the reason it is
// flagged under.
type Ignored struct{ Reason string }

func (e *Ignored) Error() string { return e.Reason }

func ignore(format string, args ...any) error {
	return &Ignored{Reason: fmt.Sprintf(format, args...)}
}

// ApplyRemote adds one file from a network's repo to this site's index. Every
// file is checked against who signed the commit that added it: its site must
// be the signer's, and only the site that wrote a post may revise or retract
// it. Applying a file already in the index changes nothing, so a pull can be
// repeated (reindex).
func (s *Service) ApplyRemote(ctx context.Context, f RemoteFile) error {
	if _, ok := s.network(f.Network); !ok {
		return ignore("network %s is not configured here", f.Network)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now().UTC()
	switch f.File.Kind {
	case "board":
		return s.applyBoard(ctx, f, now)
	case "thread":
		return s.applyThread(ctx, f)
	case "post":
		return s.applyPost(ctx, f, now)
	case "event":
		return s.applyEvent(ctx, f, now)
	case "blob":
		return s.applyBlob(ctx, f)
	}
	return ignore("not a crew-board file")
}

func (s *Service) applyBoard(ctx context.Context, f RemoteFile, now time.Time) error {
	var head BoardFile
	body, err := ParseFrontmatter(f.Data, &head)
	if err != nil {
		return ignore("unreadable board file: %v", err)
	}
	if head.Site != f.Signer || head.Name != f.File.Board {
		return ignore("board file claims site %s and board %s, signed by %s", head.Site, head.Name, f.Signer)
	}
	if existing, err := s.store.GetBoard(ctx, head.Name); err == nil {
		if existing.Network != f.Network {
			return ignore("board %s already exists here and is not shared on %s", head.Name, f.Network)
		}
		return nil
	}
	b := Board{Name: head.Name, Site: head.Site, Description: body, CreatedAt: head.CreatedAt, CreatedBy: head.CreatedBy, Network: f.Network}
	return s.store.Apply(ctx, Receipt{}, []Change{{Board: &b}})
}

func (s *Service) sharedBoard(ctx context.Context, f RemoteFile) error {
	b, err := s.store.GetBoard(ctx, f.File.Board)
	if err != nil || b.Network != f.Network {
		return ignore("board %s is not shared on %s here", f.File.Board, f.Network)
	}
	return nil
}

func (s *Service) applyThread(ctx context.Context, f RemoteFile) error {
	if err := s.sharedBoard(ctx, f); err != nil {
		return err
	}
	var head ThreadFile
	if _, err := ParseFrontmatter(f.Data, &head); err != nil {
		return ignore("unreadable thread file: %v", err)
	}
	if head.Site != f.Signer || head.CreatedBy.Site != f.Signer || head.ID != f.File.Thread || head.Board != f.File.Board || !strings.HasPrefix(head.ID, f.Signer+"-") {
		return ignore("thread file claims site %s, signed by %s", head.Site, f.Signer)
	}
	if _, err := s.store.GetThread(ctx, head.ID); err == nil {
		return nil
	}
	t := Thread{ID: head.ID, Board: head.Board, Title: head.Title, State: ThreadOpen, OpeningPostID: head.OpeningPost, CreatedAt: head.CreatedAt, CreatedBy: head.CreatedBy, LastActivityAt: head.CreatedAt}
	return s.store.Apply(ctx, Receipt{}, []Change{{Thread: &t}})
}

func (s *Service) applyPost(ctx context.Context, f RemoteFile, now time.Time) error {
	if err := s.sharedBoard(ctx, f); err != nil {
		return err
	}
	var head PostFile
	body, err := ParseFrontmatter(f.Data, &head)
	if err != nil {
		return ignore("unreadable post file: %v", err)
	}
	if head.Site != f.Signer || head.Author.Site != f.Signer || head.ID != f.File.ID || head.Thread != f.File.Thread || head.Revision != f.File.Revision || !strings.HasPrefix(head.ID, f.Signer+"-") {
		return ignore("post file claims site %s (author %s), signed by %s", head.Site, head.Author.Site, f.Signer)
	}
	thread, err := s.store.GetThread(ctx, head.Thread)
	if err != nil {
		return ignore("post for a thread this site does not have: %s", head.Thread)
	}
	revision := Revision{PostID: head.ID, Revision: head.Revision, Body: body, Mentions: head.Mentions, CreatedAt: head.CreatedAt}
	existing, err := s.store.GetPost(ctx, head.ID)
	if head.Revision == 1 {
		if err == nil {
			return nil
		}
		post := Post{ID: head.ID, ThreadID: head.Thread, ParentID: head.Parent, Author: head.Author, Site: head.Site, State: PostVisible, Revision: 1, Body: body, Mentions: head.Mentions, Urgent: head.Urgent, Kind: head.Kind, CreatedAt: head.CreatedAt, Sync: SyncSynced}
		changes := []Change{{Post: &post}, {Revision: &revision}, {Touch: &Touch{ThreadID: thread.ID, At: maxTime(thread.LastActivityAt, head.CreatedAt)}}}
		var replyTo []Participant
		if parent, err := s.store.GetPost(ctx, head.Parent); err == nil && head.Parent != "" && parent.State == PostVisible {
			replyTo = []Participant{parent.Author.Participant()}
		}
		notices, err := s.noticesFor(ctx, now, thread, post, ReasonReply, replyTo)
		if err != nil {
			return err
		}
		changes = append(changes, s.localNotices(f, notices)...)
		return s.applyAndPoke(ctx, changes)
	}
	if err != nil {
		return ignore("revision %d of a post this site does not have: %s", head.Revision, head.ID)
	}
	if existing.Site != f.Signer {
		return ignore("revision of %s's post %s signed by %s", existing.Site, head.ID, f.Signer)
	}
	if head.Revision <= existing.Revision {
		return nil
	}
	if head.Revision != existing.Revision+1 || existing.State != PostVisible {
		return ignore("revision %d of post %s does not follow revision %d (%s)", head.Revision, head.ID, existing.Revision, existing.State)
	}
	earlier := map[Participant]bool{}
	for _, m := range existing.Mentions {
		earlier[m.Participant()] = true
	}
	var added []Mention
	for _, m := range head.Mentions {
		if !earlier[m.Participant()] {
			added = append(added, m)
		}
	}
	existing.Body, existing.Mentions, existing.Revision = body, head.Mentions, head.Revision
	changes := []Change{{Revision: &revision}, {PostState: &PostState{PostID: existing.ID, State: PostVisible, Revision: head.Revision, EditedAt: head.CreatedAt}}, {Touch: &Touch{ThreadID: thread.ID, At: maxTime(thread.LastActivityAt, head.CreatedAt)}}}
	notices, err := s.mentionNotices(ctx, now, thread, existing, added)
	if err != nil {
		return err
	}
	return s.applyAndPoke(ctx, append(changes, s.localNotices(f, notices)...))
}

func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// localNotices keeps the notices for this site's own participants: another
// site's people and seats hear from their own site. A partner's mentions and
// replies wake as this site set for that partner.
func (s *Service) localNotices(f RemoteFile, changes []Change) []Change {
	network, _ := s.network(f.Network)
	var out []Change
	for _, c := range changes {
		n := c.Notice
		if n == nil || n.Recipient.Site != s.cfg.Site {
			continue
		}
		if f.Signer != s.cfg.Site && n.Reason != ReasonSubscription {
			n.Policy = network.Partners[f.Signer]
			if n.Policy == "" {
				n.Policy = "no_wake"
			}
		}
		out = append(out, c)
	}
	return out
}

func (s *Service) applyAndPoke(ctx context.Context, changes []Change) error {
	if err := s.store.Apply(ctx, Receipt{}, changes); err != nil {
		return err
	}
	for _, c := range changes {
		if c.Notice != nil {
			s.poke()
			break
		}
	}
	return nil
}

func (s *Service) applyEvent(ctx context.Context, f RemoteFile, now time.Time) error {
	if err := s.sharedBoard(ctx, f); err != nil {
		return err
	}
	var e EventFile
	if err := json.Unmarshal(f.Data, &e); err != nil {
		return ignore("unreadable event file: %v", err)
	}
	if e.Site != f.Signer || e.ID != f.File.ID || e.Thread != f.File.Thread || !strings.HasPrefix(e.ID, f.Signer+"-") {
		return ignore("event file claims site %s, signed by %s", e.Site, f.Signer)
	}
	if existingEvent(ctx, s, e.ID) {
		return nil
	}
	thread, err := s.store.GetThread(ctx, e.Thread)
	if err != nil {
		// A purged thread is gone here already.
		return nil
	}
	event := Event{ID: e.ID, ThreadID: e.Thread, PostID: e.Post, Type: e.Type, Note: e.Note, Actor: e.Actor, Data: e.Data, CreatedAt: e.CreatedAt}
	str := func(key string) string { v, _ := e.Data[key].(string); return v }
	update := ThreadUpdate{ID: thread.ID, State: thread.State, Title: thread.Title, Links: thread.Links, Aliases: thread.Aliases, RedirectTo: thread.RedirectTo}
	var changes []Change
	switch e.Type {
	case "retract":
		post, err := s.store.GetPost(ctx, e.Post)
		if err != nil || post.State != PostVisible {
			return nil
		}
		if post.Site != f.Signer {
			return ignore("retraction of %s's post %s signed by %s", post.Site, post.ID, f.Signer)
		}
		changes = []Change{{PostState: &PostState{PostID: post.ID, State: PostRetracted, Revision: post.Revision, Note: e.Note, EditedAt: post.EditedAt}}}
	case "purge":
		if e.Post == "" || e.Post == thread.OpeningPostID {
			changes = []Change{{ScrubThread: thread.ID}}
		} else {
			changes = []Change{{ScrubPost: e.Post}}
		}
	case "settle":
		into := str("into")
		if !linkRef.MatchString(into) {
			return ignore("settle into %q", into)
		}
		if !slices.ContainsFunc(update.Links, func(l Link) bool { return l.Ref == into }) {
			update.Links = append(update.Links, Link{Ref: into, Note: e.Note, By: e.Actor, At: e.CreatedAt})
		}
		update.State = ThreadSettled
		changes = []Change{{ThreadUpdate: &update}}
	case "rename":
		title := strings.TrimSpace(str("title"))
		if title == "" || len(title) > maxTitle {
			return ignore("rename to %q", title)
		}
		if title != update.Title {
			if !slices.Contains(update.Aliases, update.Title) {
				update.Aliases = append(update.Aliases, update.Title)
			}
			update.Title = title
		}
		changes = []Change{{ThreadUpdate: &update}}
	case "close":
		update.State, update.RedirectTo = ThreadClosed, str("redirect")
		changes = []Change{{ThreadUpdate: &update}}
	case "ack":
		raw, _ := json.Marshal(e.Data["for"])
		var who Participant
		if json.Unmarshal(raw, &who) != nil || who.Site != f.Signer {
			return ignore("acknowledgement for %v signed by %s", e.Data["for"], f.Signer)
		}
		changes = []Change{{Ack: &AckChange{PostID: e.Post, For: who, By: e.Actor, At: e.CreatedAt}}}
	default:
		return ignore("event type %q is not shared", e.Type)
	}
	if e.Type != "purge" {
		changes = append([]Change{{Event: &event}}, changes...)
	} else {
		// The purge event itself keeps only who purged and when.
		event.Note = ""
		changes = append(changes, Change{Event: &event})
	}
	return s.store.Apply(ctx, Receipt{}, changes)
}

func existingEvent(ctx context.Context, s *Service, id string) bool {
	type eventChecker interface {
		HasEvent(ctx context.Context, id string) bool
	}
	if c, ok := s.store.(eventChecker); ok {
		return c.HasEvent(ctx, id)
	}
	return false
}

// applyBlob re-runs this site's own ingest on an attachment another site
// shared, and files the result under the reference posts use for it.
func (s *Service) applyBlob(ctx context.Context, f RemoteFile) error {
	if s.media == nil {
		return ignore("attachments are not set up on this site")
	}
	if digest(f.Data) != f.File.ID {
		return ignore("blob does not hash to its name")
	}
	if a, err := s.attachments.GetAttachment(ctx, f.File.ID); err == nil && a.State != AttachmentDropped {
		return nil
	}
	a, err := s.media.Ingest(ctx, f.File.ID+f.File.Ext, f.Data, Skip{})
	if err != nil {
		return ignore("attachment refused here: %v", err)
	}
	if a.State == AttachmentHeld {
		a.Bytes = int64(len(f.Data))
	}
	a.Ref = f.File.ID
	a.Checks = append(a.Checks, "shared by site "+f.Signer+"; re-checked here")
	return s.attachments.PutAttachment(ctx, a)
}

// ShareAck queues a local acknowledgement on a shared board as an event.
func (s *Service) shareAck(ctx context.Context, post Post, thread Thread, who Author, now time.Time) []Change {
	network := s.boardNetwork(ctx, thread.Board, nil)
	if network == "" {
		return nil
	}
	event := Event{ID: NewID(s.cfg.Site, now), ThreadID: thread.ID, PostID: post.ID, Type: "ack", Actor: who, CreatedAt: now, Data: map[string]any{"for": who.Participant()}}
	return []Change{{Event: &event}, {Outbox: &OutboxFile{Network: network, Path: EventFilePath(thread.Board, thread.ID, event.ID), Content: EncodeEvent(thread.Board, event, s.cfg.Site)}}}
}

// Networks lists this site's networks with their sync state.
func (s *Service) Networks(ctx context.Context) ([]SyncState, error) {
	shared, ok := s.store.(SharedStore)
	if !ok {
		return []SyncState{}, nil
	}
	out := []SyncState{}
	for _, n := range s.cfg.Networks {
		st, err := shared.GetSyncState(ctx, n.Name)
		if err != nil && !errors.Is(err, ErrNotFound) {
			return nil, err
		}
		st.Network = n.Name
		out = append(out, st)
	}
	return out, nil
}
