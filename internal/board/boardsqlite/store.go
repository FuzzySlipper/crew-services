// Package boardsqlite is crew-board's SQLite store.
package boardsqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"crew-services/internal/board"

	_ "modernc.org/sqlite"
)

//go:embed migrations/001_board.sql
var migration001 string

//go:embed migrations/002_notices.sql
var migration002 string

//go:embed migrations/003_attachments.sql
var migration003 string

//go:embed migrations/004_curation.sql
var migration004 string

//go:embed migrations/005_review_fixes.sql
var migration005 string

//go:embed migrations/006_purge_notices.sql
var migration006 string

//go:embed migrations/007_shared.sql
var migration007 string

//go:embed migrations/008_joined.sql
var migration008 string

//go:embed migrations/009_purged_blobs.sql
var migration009 string

//go:embed migrations/010_revision_model.sql
var migration010 string

var migrations = []string{migration001, migration002, migration003, migration004, migration005, migration006, migration007, migration008, migration009, migration010}

// Store is crew-board's records in one SQLite file.
type Store struct {
	db *sql.DB
}

// Open opens path and applies every migration it has not seen.
func Open(ctx context.Context, path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open board database: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	for _, stmt := range []string{"PRAGMA foreign_keys = ON", "PRAGMA journal_mode = WAL", `CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY, applied_at TEXT NOT NULL)`} {
		if _, err := s.db.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("prepare board database: %w", err)
		}
	}
	for i, script := range migrations {
		version := i + 1
		var seen int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&seen); err != nil {
			return err
		}
		if seen > 0 {
			continue
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, script); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("board migration %d: %w", version, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, applied_at) VALUES (?, ?)`, version, stamp(time.Now())); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

// stamp is a fixed-width UTC time, so stored times sort as text.
func stamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }

func parseStamp(value string) time.Time {
	t, _ := time.Parse("2006-01-02T15:04:05.000000000Z", value)
	return t
}

func encode(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (s *Store) Receipt(ctx context.Context, operationID string) (board.Receipt, error) {
	var r board.Receipt
	var result, created string
	err := s.db.QueryRowContext(ctx, `SELECT operation_id, request_hash, result, created_at, purged FROM operations WHERE operation_id = ?`, operationID).Scan(&r.OperationID, &r.RequestHash, &result, &created, &r.Purged)
	if errors.Is(err, sql.ErrNoRows) {
		return board.Receipt{}, board.ErrNotFound
	}
	if err != nil {
		return board.Receipt{}, err
	}
	r.Result, r.CreatedAt = json.RawMessage(result), parseStamp(created)
	return r, nil
}

func (s *Store) Apply(ctx context.Context, receipt board.Receipt, changes []board.Change) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range changes {
		if err := apply(ctx, tx, c); err != nil {
			return err
		}
	}
	if receipt.OperationID != "" {
		if _, err := tx.ExecContext(ctx, `INSERT INTO operations (operation_id, request_hash, result, created_at) VALUES (?, ?, ?, ?)`, receipt.OperationID, receipt.RequestHash, string(receipt.Result), stamp(receipt.CreatedAt)); err != nil {
			return fmt.Errorf("store receipt: %w", err)
		}
	}
	return tx.Commit()
}

func apply(ctx context.Context, tx *sql.Tx, c board.Change) error {
	var err error
	switch {
	case c.Board != nil:
		b := c.Board
		_, err = tx.ExecContext(ctx, `INSERT INTO boards (name, site, description, created_at, created_by, network) VALUES (?, ?, ?, ?, ?, ?)`, b.Name, b.Site, b.Description, stamp(b.CreatedAt), encode(b.CreatedBy), b.Network)
	case c.Outbox != nil:
		o := c.Outbox
		if _, err = tx.ExecContext(ctx, `INSERT INTO sync_outbox (network, path, content, post_id, created_at) VALUES (?, ?, ?, ?, ?)`, o.Network, o.Path, o.Content, o.PostID, stamp(time.Now())); err == nil && o.PostID != "" && strings.Contains(o.Path, "/posts/") {
			_, err = tx.ExecContext(ctx, `UPDATE posts SET sync = 'pending_push' WHERE id = ?`, o.PostID)
		}
	case c.Thread != nil:
		t := c.Thread
		_, err = tx.ExecContext(ctx, `INSERT INTO threads (id, board, title, state, opening_post_id, created_at, created_by, last_activity_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, t.Board, t.Title, t.State, t.OpeningPostID, stamp(t.CreatedAt), encode(t.CreatedBy), stamp(t.LastActivityAt))
	case c.Post != nil:
		p := c.Post
		_, err = tx.ExecContext(ctx, `INSERT INTO posts (id, thread_id, parent_id, author, site, state, revision, note, urgent, kind, sync, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?, ?, ?, ?)`, p.ID, p.ThreadID, p.ParentID, encode(p.Author), p.Site, p.State, p.Revision, p.Note, p.Urgent, p.Kind, p.Sync, stamp(p.CreatedAt))
	case c.ThreadUpdate != nil:
		t := c.ThreadUpdate
		_, err = tx.ExecContext(ctx, `UPDATE threads SET state = ?, title = ?, links = ?, aliases = ?, redirect_to = ? WHERE id = ?`, t.State, t.Title, encode(nonNilLinks(t.Links)), encode(nonNilStrings(t.Aliases)), t.RedirectTo, t.ID)
	case c.Ack != nil:
		a := c.Ack
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO acks (post_id, participant_kind, participant_address, participant_site, acked_by, created_at) VALUES (?, ?, ?, ?, ?, ?)`, a.PostID, a.For.Kind, a.For.Address, a.For.Site, encode(a.By), stamp(a.At))
	case c.Notice != nil:
		n := c.Notice
		// One notice per post and participant: a later reason for the same post
		// (an edit's new mention of a follower) keeps the first notice.
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO notices (id, post_id, thread_id, recipient_kind, recipient_address, recipient_site, reason, policy, urgent, state, next_attempt_at, last_error, created_at, give_up_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			n.ID, n.PostID, n.ThreadID, n.Recipient.Kind, n.Recipient.Address, n.Recipient.Site, n.Reason, n.Policy, n.Urgent, n.State, stamp(n.NextAttemptAt), n.LastError, stamp(n.CreatedAt), stamp(n.GiveUpAt))
	case c.Subscribe != nil:
		kind, target := subscriptionTarget(*c.Subscribe)
		p := c.Subscribe.Participant
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO subscriptions (participant_kind, participant_address, participant_site, target_kind, target, created_at) VALUES (?, ?, ?, ?, ?, ?)`, p.Kind, p.Address, p.Site, kind, target, stamp(c.Subscribe.CreatedAt))
	case c.Revision != nil:
		r := c.Revision
		_, err = tx.ExecContext(ctx, `INSERT INTO revisions (post_id, revision, body, mentions, model, created_at) VALUES (?, ?, ?, ?, ?, ?)`, r.PostID, r.Revision, r.Body, encode(nonNil(r.Mentions)), r.Model, stamp(r.CreatedAt))
	case c.Event != nil:
		e := c.Event
		_, err = tx.ExecContext(ctx, `INSERT INTO events (id, thread_id, post_id, type, note, actor, data, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?)`, e.ID, e.ThreadID, e.PostID, e.Type, e.Note, encode(e.Actor), encode(e.Data), stamp(e.CreatedAt))
	case c.Touch != nil:
		_, err = tx.ExecContext(ctx, `UPDATE threads SET last_activity_at = ? WHERE id = ?`, stamp(c.Touch.At), c.Touch.ThreadID)
	case c.PostState != nil:
		p := c.PostState
		edited := sql.NullString{}
		if !p.EditedAt.IsZero() {
			edited = sql.NullString{String: stamp(p.EditedAt), Valid: true}
		}
		_, err = tx.ExecContext(ctx, `UPDATE posts SET state = ?, revision = ?, note = ?, edited_at = ? WHERE id = ?`, p.State, p.Revision, p.Note, edited, p.PostID)
	case c.ScrubPost != "":
		if err = scrubReceipts(ctx, tx, `SELECT ?`, c.ScrubPost); err == nil {
			err = scrub(ctx, tx, `post_id = ?`, `id = ?`, c.ScrubPost)
		}
		if err == nil {
			err = scrubOutbox(ctx, tx, `post_id = ? AND path LIKE '%/posts/%'`, c.ScrubPost)
		}
	case c.ScrubThread != "":
		// Receipts are scrubbed first, while the thread's post IDs are known.
		if err = scrubReceipts(ctx, tx, `SELECT id FROM posts WHERE thread_id = ?1 UNION SELECT ?1`, c.ScrubThread); err == nil {
			err = scrub(ctx, tx, `post_id IN (SELECT id FROM posts WHERE thread_id = ?)`, `thread_id = ?`, c.ScrubThread)
		}
		if err == nil {
			// Thread-level events (renames, settles, closes) carry titles and
			// notes but no post; they go with the thread.
			_, err = tx.ExecContext(ctx, `UPDATE events SET note = '', data = '{}', actor = CASE WHEN type = 'purge' THEN actor ELSE '{}' END WHERE thread_id = ?`, c.ScrubThread)
		}
		if err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE threads SET purged = 1, title = '', created_by = '{}', links = '[]', aliases = '[]', redirect_to = '' WHERE id = ?`, c.ScrubThread)
		}
		if err == nil {
			err = scrubOutbox(ctx, tx, `path LIKE '%/threads/' || ? || '/%'`, c.ScrubThread)
		}
	default:
		err = errors.New("empty change")
	}
	if err != nil {
		return fmt.Errorf("apply change: %w", err)
	}
	return nil
}

// scrubReceipts empties the stored result of every receipt that carries one
// of the IDs ids selects (a purged post, or a purged thread and its posts).
// The receipt itself stays, marked purged, so a retry of the same operation
// is refused instead of writing again.
func scrubReceipts(ctx context.Context, tx *sql.Tx, ids string, arg string) error {
	rows, err := tx.QueryContext(ctx, ids, arg)
	if err != nil {
		return err
	}
	var targets []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		targets = append(targets, id)
	}
	rows.Close()
	for _, id := range targets {
		if _, err := tx.ExecContext(ctx, `UPDATE operations SET result = '{}', purged = 1 WHERE result LIKE ?`, `%"`+id+`"%`); err != nil {
			return err
		}
	}
	return nil
}

// scrubOutbox drops repo files not yet pushed for what is purged, and empties
// the stored copy of those already pushed (the repo keeps its own history;
// see docs/crew-board.md, Security).
func scrubOutbox(ctx context.Context, tx *sql.Tx, where, arg string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM sync_outbox WHERE state = 'pending' AND `+where, arg); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET content = '' WHERE `+where, arg)
	return err
}

// scrub removes authored text and identity from the posts whereRevision and
// wherePost select, leaving content-free tombstones. Purge events keep only
// who purged and when.
func scrub(ctx context.Context, tx *sql.Tx, whereRevision, wherePost, arg string) error {
	for _, stmt := range []string{
		`UPDATE revisions SET body = '', mentions = '[]' WHERE ` + whereRevision,
		`UPDATE posts SET state = 'purged', author = '{}', note = '' WHERE ` + wherePost,
		`UPDATE events SET note = '', data = '{}', actor = '{}' WHERE type != 'purge' AND ` + whereRevision,
		// Every notice about the post loses its cached text, whatever its
		// state; one not yet sent never will be.
		`UPDATE notices SET body = '', purged = 1, next_attempt_at = '9999-01-01T00:00:00.000000000Z', state = CASE WHEN state = 'pending' THEN 'failed' ELSE state END,
			last_error = CASE WHEN state = 'pending' THEN 'post purged before the notice went out' ELSE last_error END WHERE ` + whereRevision,
	} {
		if _, err := tx.ExecContext(ctx, stmt, arg); err != nil {
			return err
		}
	}
	return nil
}

func nonNilLinks(l []board.Link) []board.Link {
	if l == nil {
		return []board.Link{}
	}
	return l
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func nonNil(m []board.Mention) []board.Mention {
	if m == nil {
		return []board.Mention{}
	}
	return m
}

const boardColumns = `b.name, b.site, b.description, b.created_at, b.created_by, (SELECT COUNT(*) FROM threads t WHERE t.board = b.name AND t.purged = 0), b.network`

func scanBoard(row interface{ Scan(...any) error }) (board.Board, error) {
	var b board.Board
	var created, by string
	if err := row.Scan(&b.Name, &b.Site, &b.Description, &created, &by, &b.Threads, &b.Network); err != nil {
		return board.Board{}, err
	}
	b.CreatedAt = parseStamp(created)
	_ = json.Unmarshal([]byte(by), &b.CreatedBy)
	return b, nil
}

func (s *Store) GetBoard(ctx context.Context, name string) (board.Board, error) {
	b, err := scanBoard(s.db.QueryRowContext(ctx, `SELECT `+boardColumns+` FROM boards b WHERE b.name = ?`, name))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Board{}, board.ErrNotFound
	}
	return b, err
}

func (s *Store) ListBoards(ctx context.Context) ([]board.Board, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boardColumns+` FROM boards b ORDER BY b.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Board{}
	for rows.Next() {
		b, err := scanBoard(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

const threadColumns = `t.id, t.board, t.title, t.state, t.opening_post_id, t.created_at, t.created_by, t.last_activity_at, (SELECT COUNT(*) FROM posts p WHERE p.thread_id = t.id AND p.state != 'purged'), t.links, t.aliases, t.redirect_to`

func scanThread(row interface{ Scan(...any) error }) (board.Thread, error) {
	var t board.Thread
	var created, by, active, links, aliases string
	if err := row.Scan(&t.ID, &t.Board, &t.Title, &t.State, &t.OpeningPostID, &created, &by, &active, &t.Posts, &links, &aliases, &t.RedirectTo); err != nil {
		return board.Thread{}, err
	}
	t.CreatedAt, t.LastActivityAt = parseStamp(created), parseStamp(active)
	_ = json.Unmarshal([]byte(by), &t.CreatedBy)
	_ = json.Unmarshal([]byte(links), &t.Links)
	_ = json.Unmarshal([]byte(aliases), &t.Aliases)
	if len(t.Links) == 0 {
		t.Links = nil
	}
	if len(t.Aliases) == 0 {
		t.Aliases = nil
	}
	// The site is the thread ID's prefix.
	site, _, _ := strings.Cut(t.ID, "-")
	t.Ref = "board:" + site + "/" + t.ID
	return t, nil
}

func (s *Store) GetThread(ctx context.Context, id string) (board.Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads t WHERE t.id = ? AND t.purged = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Thread{}, board.ErrNotFound
	}
	return t, err
}

func (s *Store) ListThreads(ctx context.Context, name string, after board.ThreadCursor, limit int, filter board.ThreadFilter) ([]board.Thread, error) {
	query := `SELECT ` + threadColumns + ` FROM threads t WHERE t.board = ? AND t.purged = 0`
	args := []any{name}
	if len(filter.States) > 0 {
		query += ` AND t.state IN (` + strings.TrimSuffix(strings.Repeat("?, ", len(filter.States)), ", ") + `)`
		for _, state := range filter.States {
			args = append(args, state)
		}
	}
	if !filter.ActiveSince.IsZero() {
		query += ` AND t.last_activity_at >= ?`
		args = append(args, stamp(filter.ActiveSince))
	}
	if after.ID != "" {
		at := stamp(after.LastActivityAt)
		query += ` AND (t.last_activity_at < ? OR (t.last_activity_at = ? AND t.id < ?))`
		args = append(args, at, at, after.ID)
	}
	query += ` ORDER BY t.last_activity_at DESC, t.id DESC LIMIT ?`
	rows, err := s.db.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Thread{}
	for rows.Next() {
		t, err := scanThread(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

const postColumns = `p.id, p.thread_id, COALESCE(p.parent_id, ''), p.author, p.site, p.state, p.revision, p.note, p.urgent, p.kind, p.sync, p.created_at, COALESCE(p.edited_at, ''), r.body, r.mentions`

const postFrom = ` FROM posts p JOIN revisions r ON r.post_id = p.id AND r.revision = p.revision`

// postKey orders posts across sites: the time written (fixed-width, so it
// sorts as text), then the ID. keyOf is the same key of a posts row selected
// on its own.
const (
	postKey = `(p.created_at || '|' || p.id)`
	keyOf   = `(created_at || '|' || id)`
)

func scanPost(row interface{ Scan(...any) error }) (board.Post, error) {
	var p board.Post
	var author, created, edited, mentions string
	if err := row.Scan(&p.ID, &p.ThreadID, &p.ParentID, &author, &p.Site, &p.State, &p.Revision, &p.Note, &p.Urgent, &p.Kind, &p.Sync, &created, &edited, &p.Body, &mentions); err != nil {
		return board.Post{}, err
	}
	p.CreatedAt = parseStamp(created)
	if edited != "" {
		p.EditedAt = parseStamp(edited)
	}
	_ = json.Unmarshal([]byte(author), &p.Author)
	_ = json.Unmarshal([]byte(mentions), &p.Mentions)
	if len(p.Mentions) == 0 {
		p.Mentions = nil
	}
	return p, nil
}

func (s *Store) GetPost(ctx context.Context, id string) (board.Post, error) {
	p, err := scanPost(s.db.QueryRowContext(ctx, `SELECT `+postColumns+postFrom+` JOIN threads t ON t.id = p.thread_id WHERE p.id = ? AND t.purged = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Post{}, board.ErrNotFound
	}
	return p, err
}

func (s *Store) ListPosts(ctx context.Context, threadID, afterID string, limit int) ([]board.Post, error) {
	// Posts are in the order they were written. IDs sort by time only within
	// one site, so the order is the time, then the ID.
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+postFrom+` WHERE p.thread_id = ? AND (? = '' OR `+postKey+` > (SELECT `+keyOf+` FROM posts WHERE id = ?)) ORDER BY p.created_at, p.id LIMIT ?`, threadID, afterID, afterID, limit)
	if err != nil {
		return nil, err
	}
	return collectPosts(rows)
}

func collectPosts(rows *sql.Rows) ([]board.Post, error) {
	defer rows.Close()
	out := []board.Post{}
	for rows.Next() {
		p, err := scanPost(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ListRevisions(ctx context.Context, postID string) ([]board.Revision, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT post_id, revision, body, mentions, model, created_at FROM revisions WHERE post_id = ? ORDER BY revision`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Revision{}
	for rows.Next() {
		var r board.Revision
		var mentions, created string
		if err := rows.Scan(&r.PostID, &r.Revision, &r.Body, &mentions, &r.Model, &created); err != nil {
			return nil, err
		}
		r.CreatedAt = parseStamp(created)
		_ = json.Unmarshal([]byte(mentions), &r.Mentions)
		if len(r.Mentions) == 0 {
			r.Mentions = nil
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) Search(ctx context.Context, name, query string, limit int) ([]board.Post, error) {
	pattern := "%" + strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query) + "%"
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+postFrom+` JOIN threads t ON t.id = p.thread_id
		WHERE t.board = ? AND t.purged = 0 AND p.state = 'visible' AND (r.body LIKE ? ESCAPE '\' OR t.title LIKE ? ESCAPE '\' OR t.aliases LIKE ? ESCAPE '\')
		ORDER BY p.created_at DESC, p.id DESC LIMIT ?`, name, pattern, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	return collectPosts(rows)
}

func subscriptionTarget(sub board.Subscription) (kind, target string) {
	if sub.Thread != "" {
		return "thread", sub.Thread
	}
	return "board", sub.Board
}

// Unsubscribe removes a subscription; one that does not exist is no error.
func (s *Store) Unsubscribe(ctx context.Context, sub board.Subscription) error {
	kind, target := subscriptionTarget(sub)
	p := sub.Participant
	_, err := s.db.ExecContext(ctx, `DELETE FROM subscriptions WHERE participant_kind = ? AND participant_address = ? AND participant_site = ? AND target_kind = ? AND target = ?`, p.Kind, p.Address, p.Site, kind, target)
	return err
}

// Subscribe adds a subscription; one that exists already is kept as it was.
func (s *Store) Subscribe(ctx context.Context, sub board.Subscription) error {
	return s.Apply(ctx, board.Receipt{}, []board.Change{{Subscribe: &sub}})
}

// Subscribers lists who follows a thread, directly or through its board.
func (s *Store) Subscribers(ctx context.Context, threadID, boardName string) ([]board.Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT participant_kind, participant_address, participant_site FROM subscriptions
		WHERE (target_kind = 'thread' AND target = ?) OR (target_kind = 'board' AND target = ?) ORDER BY 1, 2, 3`, threadID, boardName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Participant{}
	for rows.Next() {
		var p board.Participant
		if err := rows.Scan(&p.Kind, &p.Address, &p.Site); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Subscriptions lists what a participant follows.
func (s *Store) Subscriptions(ctx context.Context, p board.Participant) ([]board.Subscription, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT target_kind, target, created_at FROM subscriptions WHERE participant_kind = ? AND participant_address = ? AND participant_site = ? ORDER BY created_at`, p.Kind, p.Address, p.Site)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Subscription{}
	for rows.Next() {
		var kind, target, created string
		if err := rows.Scan(&kind, &target, &created); err != nil {
			return nil, err
		}
		sub := board.Subscription{Participant: p, CreatedAt: parseStamp(created)}
		if kind == "thread" {
			sub.Thread = target
		} else {
			sub.Board = target
		}
		out = append(out, sub)
	}
	return out, rows.Err()
}

// MarkRead moves a participant's cursor in a thread forward to postID; it
// never moves back.
func (s *Store) MarkRead(ctx context.Context, p board.Participant, threadID, postID string, at time.Time) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO read_cursors (participant_kind, participant_address, participant_site, thread_id, post_id, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (participant_kind, participant_address, participant_site, thread_id) DO UPDATE SET post_id = excluded.post_id, updated_at = excluded.updated_at
		WHERE (SELECT `+keyOf+` FROM posts WHERE id = excluded.post_id) > (SELECT `+keyOf+` FROM posts WHERE id = read_cursors.post_id)`,
		p.Kind, p.Address, p.Site, threadID, postID, stamp(at))
	return err
}

// Cursor is the newest post a participant has read in a thread, or "".
func (s *Store) Cursor(ctx context.Context, p board.Participant, threadID string) (string, error) {
	var post string
	err := s.db.QueryRowContext(ctx, `SELECT post_id FROM read_cursors WHERE participant_kind = ? AND participant_address = ? AND participant_site = ? AND thread_id = ?`, p.Kind, p.Address, p.Site, threadID).Scan(&post)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return post, err
}

// Unread lists the threads with visible posts a participant was told about
// and has not read past, those that mention it first, then the newest.
func (s *Store) Unread(ctx context.Context, p board.Participant, limit int) ([]board.Unread, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT n.thread_id, COUNT(DISTINCT n.post_id), SUM(n.reason = 'mention'), MAX(`+postKey+`), MAX(p.created_at)
		FROM notices n
		JOIN threads t ON t.id = n.thread_id AND t.purged = 0
		JOIN posts p ON p.id = n.post_id AND p.state = 'visible'
		LEFT JOIN read_cursors c ON c.participant_kind = n.recipient_kind AND c.participant_address = n.recipient_address AND c.participant_site = n.recipient_site AND c.thread_id = n.thread_id
		WHERE n.recipient_kind = ? AND n.recipient_address = ? AND n.recipient_site = ? AND (c.post_id IS NULL OR `+postKey+` > (SELECT `+keyOf+` FROM posts WHERE id = c.post_id))
		GROUP BY n.thread_id ORDER BY SUM(n.reason = 'mention') > 0 DESC, MAX(`+postKey+`) DESC LIMIT ?`, p.Kind, p.Address, p.Site, limit)
	if err != nil {
		return nil, err
	}
	var out []board.Unread
	for rows.Next() {
		var u board.Unread
		var newestAt string
		if err := rows.Scan(&u.Thread.ID, &u.Posts, &u.Mentions, &u.NewestID, &newestAt); err != nil {
			rows.Close()
			return nil, err
		}
		_, u.NewestID, _ = strings.Cut(u.NewestID, "|")
		u.NewestAt = parseStamp(newestAt)
		out = append(out, u)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		thread, err := s.GetThread(ctx, out[i].Thread.ID)
		if err != nil {
			return nil, err
		}
		out[i].Thread = thread
	}
	if out == nil {
		out = []board.Unread{}
	}
	return out, nil
}

const noticeColumns = `id, post_id, thread_id, recipient_kind, recipient_address, recipient_site, reason, policy, urgent, state, attempts, next_attempt_at, last_error, message_id, created_at, give_up_at, COALESCE(sent_at, ''), body`

func scanNotice(row interface{ Scan(...any) error }) (board.Notice, error) {
	var n board.Notice
	var next, created, giveUp, sent string
	if err := row.Scan(&n.ID, &n.PostID, &n.ThreadID, &n.Recipient.Kind, &n.Recipient.Address, &n.Recipient.Site, &n.Reason, &n.Policy, &n.Urgent, &n.State, &n.Attempts, &next, &n.LastError, &n.MessageID, &created, &giveUp, &sent, &n.Body); err != nil {
		return board.Notice{}, err
	}
	n.NextAttemptAt, n.CreatedAt, n.GiveUpAt = parseStamp(next), parseStamp(created), parseStamp(giveUp)
	if sent != "" {
		n.SentAt = parseStamp(sent)
	}
	return n, nil
}

func (s *Store) notices(ctx context.Context, query string, args ...any) ([]board.Notice, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+noticeColumns+` FROM notices `+query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Notice{}
	for rows.Next() {
		n, err := scanNotice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// DueNotices lists notices with work due, oldest first: pending ones to send,
// and sent ones whose delivery is to be checked.
func (s *Store) DueNotices(ctx context.Context, now time.Time, limit int) ([]board.Notice, error) {
	// A purged notice is never worked on again: it has no text left to send,
	// and its row may not change.
	return s.notices(ctx, `WHERE state IN ('pending', 'sent') AND purged = 0 AND next_attempt_at <= ? ORDER BY next_attempt_at, id LIMIT ?`, stamp(now), limit)
}

// ListNotices lists recent notices, newest first, in one state or all.
func (s *Store) ListNotices(ctx context.Context, state string, limit int) ([]board.Notice, error) {
	if state == "" {
		return s.notices(ctx, `ORDER BY created_at DESC, id DESC LIMIT ?`, limit)
	}
	return s.notices(ctx, `WHERE state = ? ORDER BY created_at DESC, id DESC LIMIT ?`, state, limit)
}

// UpdateNotice records a send attempt's outcome.
func (s *Store) UpdateNotice(ctx context.Context, n board.Notice) error {
	sent := sql.NullString{}
	if !n.SentAt.IsZero() {
		sent = sql.NullString{String: stamp(n.SentAt), Valid: true}
	}
	_, err := s.db.ExecContext(ctx, `UPDATE notices SET state = ?, attempts = ?, next_attempt_at = ?, last_error = ?, message_id = ?, sent_at = ?, body = ?, give_up_at = ? WHERE id = ? AND purged = 0`, n.State, n.Attempts, stamp(n.NextAttemptAt), n.LastError, n.MessageID, sent, n.Body, stamp(n.GiveUpAt), n.ID)
	return err
}

const attachmentColumns = `ref, original_hash, output_hash, name, state, reason, kind, type, width, height, duration_ms, bytes, checks, created_at`

func scanAttachment(row interface{ Scan(...any) error }) (board.Attachment, error) {
	var a board.Attachment
	var checks, created string
	if err := row.Scan(&a.Ref, &a.OriginalHash, &a.OutputHash, &a.Name, &a.State, &a.Reason, &a.Kind, &a.Type, &a.Width, &a.Height, &a.DurationMS, &a.Bytes, &checks, &created); err != nil {
		return board.Attachment{}, err
	}
	a.CreatedAt = parseStamp(created)
	_ = json.Unmarshal([]byte(checks), &a.Checks)
	return a, nil
}

// PutAttachment records an attachment, replacing the record with its ref.
func (s *Store) PutAttachment(ctx context.Context, a board.Attachment) error {
	checks := a.Checks
	if checks == nil {
		checks = []string{}
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO attachments (`+attachmentColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Ref, a.OriginalHash, a.OutputHash, a.Name, a.State, a.Reason, a.Kind, a.Type, a.Width, a.Height, a.DurationMS, a.Bytes, encode(checks), stamp(a.CreatedAt))
	return err
}

func (s *Store) GetAttachment(ctx context.Context, ref string) (board.Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE ref = ?`, ref))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Attachment{}, board.ErrNotFound
	}
	return a, err
}

// AttachmentByOriginal finds an upload of the same bytes seen before.
func (s *Store) AttachmentByOriginal(ctx context.Context, original string) (board.Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE original_hash = ? AND state != 'dropped' ORDER BY created_at LIMIT 1`, original))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Attachment{}, board.ErrNotFound
	}
	return a, err
}

func (s *Store) ListAttachments(ctx context.Context, state string) ([]board.Attachment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE state = ? ORDER BY created_at`, state)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Attachment{}
	for rows.Next() {
		a, err := scanAttachment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// KindPosts lists a board's visible posts of one kind, newest first, older
// than beforeID when it is given ("" board for every board).
func (s *Store) KindPosts(ctx context.Context, name, kind, beforeID string, limit int) ([]board.Post, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+postFrom+` JOIN threads t ON t.id = p.thread_id
		WHERE (? = '' OR t.board = ?) AND t.purged = 0 AND p.state = 'visible' AND p.kind = ? AND (? = '' OR `+postKey+` < (SELECT `+keyOf+` FROM posts WHERE id = ?)) ORDER BY p.created_at DESC, p.id DESC LIMIT ?`, name, name, kind, beforeID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	return collectPosts(rows)
}

// OpenQuestions lists a board's visible questions that no visible answer
// replies to, newest first.
func (s *Store) OpenQuestions(ctx context.Context, name string, limit int) ([]board.Post, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+postFrom+` JOIN threads t ON t.id = p.thread_id
		WHERE t.board = ? AND t.purged = 0 AND p.state = 'visible' AND p.kind = 'question'
		AND NOT EXISTS (SELECT 1 FROM posts a WHERE a.parent_id = p.id AND a.kind = 'answer' AND a.state = 'visible')
		ORDER BY p.created_at DESC, p.id DESC LIMIT ?`, name, limit)
	if err != nil {
		return nil, err
	}
	return collectPosts(rows)
}

// Acks lists who acknowledged a post.
func (s *Store) Acks(ctx context.Context, postID string) ([]board.Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT participant_kind, participant_address, participant_site FROM acks WHERE post_id = ? ORDER BY created_at`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Participant{}
	for rows.Next() {
		var p board.Participant
		if err := rows.Scan(&p.Kind, &p.Address, &p.Site); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingOutbox lists a network's files still to push, oldest first.
func (s *Store) PendingOutbox(ctx context.Context, network string, limit int) ([]board.OutboxFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, network, path, content, post_id FROM sync_outbox WHERE network = ? AND state = 'pending' ORDER BY id LIMIT ?`, network, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.OutboxFile{}
	for rows.Next() {
		var f board.OutboxFile
		if err := rows.Scan(&f.ID, &f.Network, &f.Path, &f.Content, &f.PostID); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// OutboxDone marks files as pushed in commit, and the posts they belong to
// synced once nothing of theirs is left to push.
func (s *Store) OutboxDone(ctx context.Context, ids []int64, commit string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET state = 'pushed', commit_id = ? WHERE id = ?`, commit, id); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE posts SET sync = 'synced' WHERE sync = 'pending_push' AND NOT EXISTS (SELECT 1 FROM sync_outbox o WHERE o.post_id = posts.id AND o.state = 'pending')`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) GetSyncState(ctx context.Context, network string) (board.SyncState, error) {
	var st board.SyncState
	var checked, joined string
	err := s.db.QueryRowContext(ctx, `SELECT network, last_commit, status, checked_at, repo_bytes, joined_at FROM sync_state WHERE network = ?`, network).Scan(&st.Network, &st.LastCommit, &st.Status, &checked, &st.RepoBytes, &joined)
	if joined != "" {
		st.JoinedAt = parseStamp(joined)
	}
	if errors.Is(err, sql.ErrNoRows) {
		err = board.ErrNotFound
	}
	if checked != "" {
		st.CheckedAt = parseStamp(checked)
	}
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sync_outbox WHERE network = ? AND state = 'pending'`, network).Scan(&st.Pending)
	return st, err
}

func (s *Store) PutSyncState(ctx context.Context, st board.SyncState) error {
	joined := ""
	if !st.JoinedAt.IsZero() {
		joined = stamp(st.JoinedAt)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_state (network, last_commit, status, checked_at, repo_bytes, joined_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (network) DO UPDATE SET last_commit = excluded.last_commit, status = excluded.status, checked_at = excluded.checked_at, repo_bytes = excluded.repo_bytes,
		joined_at = CASE WHEN sync_state.joined_at = '' THEN excluded.joined_at ELSE sync_state.joined_at END`,
		st.Network, st.LastCommit, st.Status, stamp(st.CheckedAt), st.RepoBytes, joined)
	return err
}

func (s *Store) AddFlag(ctx context.Context, f board.SyncFlag) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sync_flags (network, commit_id, path, reason, created_at) VALUES (?, ?, ?, ?, ?)`, f.Network, f.Commit, f.Path, f.Reason, stamp(f.CreatedAt))
	return err
}

func (s *Store) ListFlags(ctx context.Context, network string, limit int) ([]board.SyncFlag, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT network, commit_id, path, reason, created_at FROM sync_flags WHERE network = ? ORDER BY id DESC LIMIT ?`, network, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.SyncFlag{}
	for rows.Next() {
		var f board.SyncFlag
		var created string
		if err := rows.Scan(&f.Network, &f.Commit, &f.Path, &f.Reason, &created); err != nil {
			return nil, err
		}
		f.CreatedAt = parseStamp(created)
		out = append(out, f)
	}
	return out, rows.Err()
}

// HasEvent reports whether an event is in the index already.
func (s *Store) HasEvent(ctx context.Context, id string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE id = ?`, id).Scan(&n)
	return n > 0
}

// AttachmentByOutput finds an attachment by the hash of its transformed
// bytes.
func (s *Store) AttachmentByOutput(ctx context.Context, output string) (board.Attachment, error) {
	a, err := scanAttachment(s.db.QueryRowContext(ctx, `SELECT `+attachmentColumns+` FROM attachments WHERE output_hash = ? AND state != 'dropped' LIMIT 1`, output))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Attachment{}, board.ErrNotFound
	}
	return a, err
}

// AttachmentReferenced reports whether any post that is not purged still
// shows the attachment, in any revision.
func (s *Store) AttachmentReferenced(ctx context.Context, ref string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM revisions r JOIN posts p ON p.id = r.post_id WHERE p.state != 'purged' AND r.body LIKE ?`, "%blob:sha256-"+ref+"%").Scan(&n)
	return n > 0, err
}

// DeleteAttachment removes an attachment's record, and the queued or stored
// repo copies of its bytes.
func (s *Store) DeleteAttachment(ctx context.Context, a board.Attachment) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM attachments WHERE ref = ?`, a.Ref); err != nil {
		return err
	}
	for _, hash := range []string{a.Ref, a.OutputHash} {
		if hash == "" {
			continue
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO purged_blobs (hash, created_at) VALUES (?, ?)`, hash, stamp(time.Now())); err != nil {
			return err
		}
		pattern := "blobs/sha256/" + hash[:2] + "/" + hash + "%"
		if _, err := tx.ExecContext(ctx, `DELETE FROM sync_outbox WHERE state = 'pending' AND path LIKE ?`, pattern); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE sync_outbox SET content = '' WHERE path LIKE ?`, pattern); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// BlobPurged reports whether a purge removed the attachment with this hash.
func (s *Store) BlobPurged(ctx context.Context, hash string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM purged_blobs WHERE hash = ?`, hash).Scan(&n)
	return n > 0
}

// ThreadPurged reports whether a thread is in the index only as purged.
func (s *Store) ThreadPurged(ctx context.Context, id string) bool {
	var n int
	_ = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM threads WHERE id = ? AND purged = 1`, id).Scan(&n)
	return n > 0
}
