package board

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The shared-board repo layout (docs/crew-board.md, Shared boards across
// sites). Every write adds files; none is ever changed.
//
//	boards/<board>/board.md
//	boards/<board>/threads/<thread>/thread.md
//	boards/<board>/threads/<thread>/posts/<post>.md          revision 1
//	boards/<board>/threads/<thread>/posts/<post>.r<N>.md     revision N
//	boards/<board>/threads/<thread>/events/<event>.yml
//	blobs/sha256/<2>/<hash>.<ext>
//
// A .md file is frontmatter between --- lines, then Markdown. The frontmatter
// and the .yml files are JSON, which is also YAML.

func BoardFilePath(board string) string { return path.Join("boards", board, "board.md") }

func ThreadFilePath(board, thread string) string {
	return path.Join("boards", board, "threads", thread, "thread.md")
}

func PostFilePath(board, thread, post string, revision int) string {
	name := post + ".md"
	if revision > 1 {
		name = fmt.Sprintf("%s.r%d.md", post, revision)
	}
	return path.Join("boards", board, "threads", thread, "posts", name)
}

func EventFilePath(board, thread, event string) string {
	return path.Join("boards", board, "threads", thread, "events", event+".yml")
}

func BlobFilePath(ref, ext string) string {
	return path.Join("blobs", "sha256", ref[:2], ref+ext)
}

// BoardFile is board.md's frontmatter; the description is its body.
type BoardFile struct {
	Name      string    `json:"name"`
	Site      string    `json:"site"`
	CreatedBy Author    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ThreadFile is thread.md's frontmatter.
type ThreadFile struct {
	ID          string    `json:"id"`
	Board       string    `json:"board"`
	Title       string    `json:"title"`
	OpeningPost string    `json:"opening_post"`
	Site        string    `json:"site"`
	CreatedBy   Author    `json:"created_by"`
	CreatedAt   time.Time `json:"created_at"`
}

// PostFile is a post revision's frontmatter; the text is its body.
type PostFile struct {
	ID        string    `json:"id"`
	Thread    string    `json:"thread"`
	Board     string    `json:"board"`
	Parent    string    `json:"parent,omitempty"`
	Site      string    `json:"site"`
	Author    Author    `json:"author"`
	Kind      string    `json:"kind,omitempty"`
	Urgent    bool      `json:"urgent,omitempty"`
	Revision  int       `json:"revision"`
	Mentions  []Mention `json:"mentions,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// EventFile is an event: retract, purge, settle, rename, close or ack.
type EventFile struct {
	ID        string         `json:"id"`
	Board     string         `json:"board"`
	Thread    string         `json:"thread"`
	Post      string         `json:"post,omitempty"`
	Type      string         `json:"type"`
	Note      string         `json:"note,omitempty"`
	Actor     Author         `json:"actor"`
	Data      map[string]any `json:"data,omitempty"`
	Site      string         `json:"site"`
	CreatedAt time.Time      `json:"created_at"`
}

func frontmatter(head any, body string) []byte {
	encoded, _ := json.MarshalIndent(head, "", "  ")
	var b bytes.Buffer
	b.WriteString("---\n")
	b.Write(encoded)
	b.WriteString("\n---\n")
	b.WriteString(body)
	if body != "" && !strings.HasSuffix(body, "\n") {
		b.WriteString("\n")
	}
	return b.Bytes()
}

// ParseFrontmatter splits a .md file into its frontmatter, decoded into head,
// and its body.
func ParseFrontmatter(data []byte, head any) (string, error) {
	text := string(data)
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", errors.New("no frontmatter")
	}
	front, body, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		return "", errors.New("unterminated frontmatter")
	}
	if err := json.Unmarshal([]byte(front), head); err != nil {
		return "", fmt.Errorf("frontmatter: %w", err)
	}
	return strings.TrimSuffix(body, "\n"), nil
}

func EncodeBoard(b Board) []byte {
	return frontmatter(BoardFile{Name: b.Name, Site: b.Site, CreatedBy: b.CreatedBy, CreatedAt: b.CreatedAt}, b.Description)
}

func EncodeThread(t Thread) []byte {
	return frontmatter(ThreadFile{ID: t.ID, Board: t.Board, Title: t.Title, OpeningPost: t.OpeningPostID, Site: t.CreatedBy.Site, CreatedBy: t.CreatedBy, CreatedAt: t.CreatedAt}, "")
}

func EncodePost(board string, p Post, r Revision) []byte {
	return frontmatter(PostFile{ID: p.ID, Thread: p.ThreadID, Board: board, Parent: p.ParentID, Site: p.Site, Author: p.Author, Kind: p.Kind, Urgent: p.Urgent, Revision: r.Revision, Mentions: r.Mentions, CreatedAt: r.CreatedAt}, r.Body)
}

func EncodeEvent(board string, e Event, site string) []byte {
	encoded, _ := json.MarshalIndent(EventFile{ID: e.ID, Board: board, Thread: e.ThreadID, Post: e.PostID, Type: e.Type, Note: e.Note, Actor: e.Actor, Data: e.Data, Site: site, CreatedAt: e.CreatedAt}, "", "  ")
	return append(encoded, '\n')
}

// RepoFile is one file of the shared-board layout, as a pull reads it.
type RepoFile struct {
	Kind     string // board, thread, post, event or blob
	Board    string
	Thread   string
	ID       string // the post, event or blob reference
	Revision int
	Ext      string
}

var (
	boardPart    = `([a-z0-9][a-z0-9.-]{0,63})`
	idPart       = `([a-z0-9][a-z0-9-]{0,31}-[0-9A-Z]{26})`
	boardFileRe  = regexp.MustCompile(`^boards/` + boardPart + `/board\.md$`)
	threadFileRe = regexp.MustCompile(`^boards/` + boardPart + `/threads/` + idPart + `/thread\.md$`)
	postFileRe   = regexp.MustCompile(`^boards/` + boardPart + `/threads/` + idPart + `/posts/` + idPart + `(?:\.r([0-9]+))?\.md$`)
	eventFileRe  = regexp.MustCompile(`^boards/` + boardPart + `/threads/` + idPart + `/events/` + idPart + `\.yml$`)
	blobFileRe   = regexp.MustCompile(`^blobs/sha256/([0-9a-f]{2})/([0-9a-f]{64})(\.webp|\.mp4)$`)
)

// ClassifyPath says what a repo path is, or false for a path outside the
// layout.
func ClassifyPath(p string) (RepoFile, bool) {
	if m := boardFileRe.FindStringSubmatch(p); m != nil {
		return RepoFile{Kind: "board", Board: m[1]}, true
	}
	if m := threadFileRe.FindStringSubmatch(p); m != nil {
		return RepoFile{Kind: "thread", Board: m[1], Thread: m[2]}, true
	}
	if m := postFileRe.FindStringSubmatch(p); m != nil {
		revision := 1
		if m[4] != "" {
			revision, _ = strconv.Atoi(m[4])
		}
		return RepoFile{Kind: "post", Board: m[1], Thread: m[2], ID: m[3], Revision: revision}, true
	}
	if m := eventFileRe.FindStringSubmatch(p); m != nil {
		return RepoFile{Kind: "event", Board: m[1], Thread: m[2], ID: m[3]}, true
	}
	if m := blobFileRe.FindStringSubmatch(p); m != nil && m[2][:2] == m[1] {
		return RepoFile{Kind: "blob", ID: m[2], Ext: m[3]}, true
	}
	return RepoFile{}, false
}
