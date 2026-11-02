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

var migrations = []string{migration001}

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
	err := s.db.QueryRowContext(ctx, `SELECT operation_id, request_hash, result, created_at FROM operations WHERE operation_id = ?`, operationID).Scan(&r.OperationID, &r.RequestHash, &result, &created)
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
	if _, err := tx.ExecContext(ctx, `INSERT INTO operations (operation_id, request_hash, result, created_at) VALUES (?, ?, ?, ?)`, receipt.OperationID, receipt.RequestHash, string(receipt.Result), stamp(receipt.CreatedAt)); err != nil {
		return fmt.Errorf("store receipt: %w", err)
	}
	return tx.Commit()
}

func apply(ctx context.Context, tx *sql.Tx, c board.Change) error {
	var err error
	switch {
	case c.Board != nil:
		b := c.Board
		_, err = tx.ExecContext(ctx, `INSERT INTO boards (name, site, description, created_at, created_by) VALUES (?, ?, ?, ?, ?)`, b.Name, b.Site, b.Description, stamp(b.CreatedAt), encode(b.CreatedBy))
	case c.Thread != nil:
		t := c.Thread
		_, err = tx.ExecContext(ctx, `INSERT INTO threads (id, board, title, state, opening_post_id, created_at, created_by, last_activity_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, t.ID, t.Board, t.Title, t.State, t.OpeningPostID, stamp(t.CreatedAt), encode(t.CreatedBy), stamp(t.LastActivityAt))
	case c.Post != nil:
		p := c.Post
		_, err = tx.ExecContext(ctx, `INSERT INTO posts (id, thread_id, parent_id, author, site, state, revision, note, created_at) VALUES (?, ?, NULLIF(?, ''), ?, ?, ?, ?, ?, ?)`, p.ID, p.ThreadID, p.ParentID, encode(p.Author), p.Site, p.State, p.Revision, p.Note, stamp(p.CreatedAt))
	case c.Revision != nil:
		r := c.Revision
		_, err = tx.ExecContext(ctx, `INSERT INTO revisions (post_id, revision, body, mentions, created_at) VALUES (?, ?, ?, ?, ?)`, r.PostID, r.Revision, r.Body, encode(nonNil(r.Mentions)), stamp(r.CreatedAt))
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
		err = scrub(ctx, tx, `post_id = ?`, `id = ?`, c.ScrubPost)
	case c.ScrubThread != "":
		if err = scrub(ctx, tx, `post_id IN (SELECT id FROM posts WHERE thread_id = ?)`, `thread_id = ?`, c.ScrubThread); err == nil {
			_, err = tx.ExecContext(ctx, `UPDATE threads SET purged = 1, title = '', created_by = '{}' WHERE id = ?`, c.ScrubThread)
		}
	default:
		err = errors.New("empty change")
	}
	if err != nil {
		return fmt.Errorf("apply change: %w", err)
	}
	return nil
}

// scrub removes authored text and identity from the posts whereRevision and
// wherePost select, leaving content-free tombstones. Purge events keep only
// who purged and when.
func scrub(ctx context.Context, tx *sql.Tx, whereRevision, wherePost, arg string) error {
	for _, stmt := range []string{
		`UPDATE revisions SET body = '', mentions = '[]' WHERE ` + whereRevision,
		`UPDATE posts SET state = 'purged', author = '{}', note = '' WHERE ` + wherePost,
		`UPDATE events SET note = '', actor = '{}' WHERE type != 'purge' AND ` + whereRevision,
	} {
		if _, err := tx.ExecContext(ctx, stmt, arg); err != nil {
			return err
		}
	}
	return nil
}

func nonNil(m []board.Mention) []board.Mention {
	if m == nil {
		return []board.Mention{}
	}
	return m
}

const boardColumns = `b.name, b.site, b.description, b.created_at, b.created_by, (SELECT COUNT(*) FROM threads t WHERE t.board = b.name AND t.purged = 0)`

func scanBoard(row interface{ Scan(...any) error }) (board.Board, error) {
	var b board.Board
	var created, by string
	if err := row.Scan(&b.Name, &b.Site, &b.Description, &created, &by, &b.Threads); err != nil {
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

const threadColumns = `t.id, t.board, t.title, t.state, t.opening_post_id, t.created_at, t.created_by, t.last_activity_at, (SELECT COUNT(*) FROM posts p WHERE p.thread_id = t.id AND p.state != 'purged')`

func scanThread(row interface{ Scan(...any) error }) (board.Thread, error) {
	var t board.Thread
	var created, by, active string
	if err := row.Scan(&t.ID, &t.Board, &t.Title, &t.State, &t.OpeningPostID, &created, &by, &active, &t.Posts); err != nil {
		return board.Thread{}, err
	}
	t.CreatedAt, t.LastActivityAt = parseStamp(created), parseStamp(active)
	_ = json.Unmarshal([]byte(by), &t.CreatedBy)
	return t, nil
}

func (s *Store) GetThread(ctx context.Context, id string) (board.Thread, error) {
	t, err := scanThread(s.db.QueryRowContext(ctx, `SELECT `+threadColumns+` FROM threads t WHERE t.id = ? AND t.purged = 0`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return board.Thread{}, board.ErrNotFound
	}
	return t, err
}

func (s *Store) ListThreads(ctx context.Context, name string, after board.ThreadCursor, limit int) ([]board.Thread, error) {
	query := `SELECT ` + threadColumns + ` FROM threads t WHERE t.board = ? AND t.purged = 0`
	args := []any{name}
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

const postColumns = `p.id, p.thread_id, COALESCE(p.parent_id, ''), p.author, p.site, p.state, p.revision, p.note, p.created_at, COALESCE(p.edited_at, ''), r.body, r.mentions`

const postFrom = ` FROM posts p JOIN revisions r ON r.post_id = p.id AND r.revision = p.revision`

func scanPost(row interface{ Scan(...any) error }) (board.Post, error) {
	var p board.Post
	var author, created, edited, mentions string
	if err := row.Scan(&p.ID, &p.ThreadID, &p.ParentID, &author, &p.Site, &p.State, &p.Revision, &p.Note, &created, &edited, &p.Body, &mentions); err != nil {
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
	rows, err := s.db.QueryContext(ctx, `SELECT `+postColumns+postFrom+` WHERE p.thread_id = ? AND p.id > ? ORDER BY p.id LIMIT ?`, threadID, afterID, limit)
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
	rows, err := s.db.QueryContext(ctx, `SELECT post_id, revision, body, mentions, created_at FROM revisions WHERE post_id = ? ORDER BY revision`, postID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []board.Revision{}
	for rows.Next() {
		var r board.Revision
		var mentions, created string
		if err := rows.Scan(&r.PostID, &r.Revision, &r.Body, &mentions, &created); err != nil {
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
		WHERE t.board = ? AND t.purged = 0 AND p.state = 'visible' AND (r.body LIKE ? ESCAPE '\' OR t.title LIKE ? ESCAPE '\')
		ORDER BY p.id DESC LIMIT ?`, name, pattern, pattern, limit)
	if err != nil {
		return nil, err
	}
	return collectPosts(rows)
}
