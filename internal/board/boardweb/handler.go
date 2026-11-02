// Package boardweb is crew-board's reading and posting surface for people:
// server-rendered pages on the LAN, dark only (docs/crew-board.md). It calls
// the board service directly, so a form writes exactly what the CLI writes.
package boardweb

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"crew-services/internal/board"
)

//go:embed static
var static embed.FS

const personCookie = "crew_board_person"

// Handler serves the pages.
type Handler struct {
	svc  *board.Service
	tmpl *template.Template
	mux  *http.ServeMux
}

// NewHandler serves crew-board's pages. Requests from outside private
// networks (the LAN, Tailscale's 100.64.0.0/10, loopback) are refused, and a
// form is accepted only from the page's own origin.
func NewHandler(svc *board.Service) http.Handler {
	h := &Handler{svc: svc, mux: http.NewServeMux()}
	h.tmpl = template.Must(template.New("").Funcs(template.FuncMap{
		"render":  RenderPost,
		"name":    displayName,
		"author":  authorName,
		"ago":     ago,
		"when":    func(t time.Time) string { return t.Local().Format("Jan 2, 15:04") },
		"opid":    newOperationID,
		"plural":  func(n int, one, many string) string { return map[bool]string{true: one, false: many}[n == 1] },
		"isReply": func(p board.Post, t board.Thread) bool { return p.ID != t.OpeningPostID },
	}).Parse(pages))
	sub, _ := fs.Sub(static, "static")
	h.mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(sub))))
	h.mux.HandleFunc("GET /who", h.who)
	h.mux.HandleFunc("POST /who", h.choose)
	h.mux.HandleFunc("GET /{$}", h.boards)
	h.mux.HandleFunc("GET /b/{board}", h.board)
	h.mux.HandleFunc("POST /b/{board}/new", h.open)
	h.mux.HandleFunc("GET /t/{thread}", h.thread)
	h.mux.HandleFunc("POST /t/{thread}/reply", h.reply)
	h.mux.HandleFunc("GET /blob/{ref}", h.blob)
	h.mux.HandleFunc("GET /held", h.held)
	h.mux.HandleFunc("POST /held/{ref}/{op}", h.release)
	return h
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !privateAddress(r.RemoteAddr) {
		http.Error(w, "crew-board is reachable from the LAN and Tailscale only", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodPost && !sameOrigin(r) {
		http.Error(w, "forms are accepted only from this board's own pages", http.StatusForbidden)
		return
	}
	header := w.Header()
	// Nothing on a page may come from anywhere but this board.
	header.Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; media-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
	header.Set("X-Content-Type-Options", "nosniff")
	// same-origin, not no-referrer: with no-referrer a browser sends
	// "Origin: null" on its own form posts, and sameOrigin refuses them.
	header.Set("Referrer-Policy", "same-origin")
	h.mux.ServeHTTP(w, r)
}

var privateNets = func() []*net.IPNet {
	var out []*net.IPNet
	for _, cidr := range []string{"127.0.0.0/8", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16", "::1/128", "fc00::/7", "fe80::/10"} {
		_, n, _ := net.ParseCIDR(cidr)
		out = append(out, n)
	}
	return out
}()

func privateAddress(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range privateNets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// sameOrigin accepts a form only when the browser says it came from this
// host's own pages: by Sec-Fetch-Site where the browser sends it, otherwise
// by Origin or Referer.
func sameOrigin(r *http.Request) bool {
	if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
		return site == "same-origin"
	}
	for _, header := range []string{"Origin", "Referer"} {
		value := r.Header.Get(header)
		if value == "" {
			continue
		}
		u, err := url.Parse(value)
		return err == nil && u.Host == r.Host
	}
	return false
}

func newOperationID() string {
	var raw [9]byte
	_, _ = rand.Read(raw[:])
	return "web/" + hex.EncodeToString(raw[:])
}

func displayName(person string) string {
	if person == "" {
		return ""
	}
	return strings.ToUpper(person[:1]) + person[1:]
}

func authorName(a board.Author) string {
	switch a.Kind {
	case board.AuthorPerson:
		return displayName(a.Address)
	case board.AuthorSeat:
		return a.Address + " · " + runtimeName(a.Adapter)
	case board.AuthorSession:
		return runtimeName(a.Adapter) + " session " + a.Address
	}
	return a.Address
}

func runtimeName(adapter string) string {
	switch adapter {
	case "claude-code":
		return "Claude Code"
	case "crew-codex":
		return "Codex"
	case "crew-dsh":
		return "DSH"
	}
	return adapter
}

func ago(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// person is who is reading and writing, from the cookie, if it names one of
// this site's people.
func (h *Handler) person(r *http.Request) string {
	c, err := r.Cookie(personCookie)
	if err != nil {
		return ""
	}
	for _, p := range h.svc.People() {
		if p == c.Value {
			return p
		}
	}
	return ""
}

type page struct {
	Title  string
	Site   string
	Person string
	Data   any
	Error  string
}

func (h *Handler) render(w http.ResponseWriter, name string, p page, status int) {
	p.Site = h.svc.Site()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := h.tmpl.ExecuteTemplate(w, name, p); err != nil {
		_, _ = io.WriteString(w, template.HTMLEscapeString(err.Error()))
	}
}

func (h *Handler) fail(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	switch board.CodeOf(err) {
	case board.CodeBoardNotFound, board.CodeThreadNotFound, board.CodePostNotFound, board.CodeAttachmentUnknown:
		status = http.StatusNotFound
	case "":
		status = http.StatusInternalServerError
	}
	h.render(w, "error", page{Title: "Something went wrong", Person: h.person(r), Error: err.Error()}, status)
}

// needPerson sends a reader who has not said who they are to the chooser.
func (h *Handler) needPerson(w http.ResponseWriter, r *http.Request) (string, bool) {
	person := h.person(r)
	if person == "" {
		http.Redirect(w, r, "/who?next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
		return "", false
	}
	return person, true
}

func (h *Handler) who(w http.ResponseWriter, r *http.Request) {
	h.render(w, "who", page{Title: "Who is reading?", Person: h.person(r), Data: map[string]any{"People": h.svc.People(), "Next": safeNext(r.URL.Query().Get("next"))}}, http.StatusOK)
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	return next
}

func (h *Handler) choose(w http.ResponseWriter, r *http.Request) {
	person := r.FormValue("person")
	for _, p := range h.svc.People() {
		if p == person {
			http.SetCookie(w, &http.Cookie{Name: personCookie, Value: p, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 365 * 24 * 3600})
			http.Redirect(w, r, safeNext(r.FormValue("next")), http.StatusSeeOther)
			return
		}
	}
	h.fail(w, r, errors.New("choose one of this site's people"))
}

func (h *Handler) boards(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	boards, err := h.svc.ListBoards(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	unread := h.unread(r.Context(), person)
	perBoard := map[string]int{}
	for _, u := range unread {
		perBoard[u.Thread.Board] += u.Posts
	}
	h.render(w, "boards", page{Title: "Boards", Person: person, Data: map[string]any{"Boards": boards, "Unread": perBoard}}, http.StatusOK)
}

func (h *Handler) unread(ctx context.Context, person string) map[string]board.Unread {
	out := map[string]board.Unread{}
	list, err := h.svc.Unread(ctx, board.Actor{Person: person}, 200)
	if err != nil {
		return out
	}
	for _, u := range list {
		out[u.Thread.ID] = u
	}
	return out
}

func (h *Handler) board(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	b, err := h.svc.GetBoard(r.Context(), r.PathValue("board"))
	if err != nil {
		h.fail(w, r, err)
		return
	}
	archived := r.URL.Query().Get("archived") == "1"
	threads, err := h.svc.ListThreads(r.Context(), b.Name, r.URL.Query().Get("after"), 50, nil, archived)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, "board", page{Title: b.Name, Person: person, Data: map[string]any{"Board": b, "Threads": threads, "Unread": h.unread(r.Context(), person), "Archived": archived}}, http.StatusOK)
}

type postView struct {
	board.Post
	Unread bool
	Parent string
}

func (h *Handler) thread(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	read, err := h.svc.ReadThread(r.Context(), r.PathValue("thread"), "", 200)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	actor := board.Actor{Person: person}
	cursor, _ := h.svc.Cursor(r.Context(), actor, read.Thread.ID)
	authors := map[string]string{}
	var posts []postView
	// Posts come in the order written; everything after the cursor is new.
	seen := cursor == ""
	for _, p := range read.Posts {
		authors[p.ID] = authorName(p.Author)
		mine := p.Author.Kind == board.AuthorPerson && p.Author.Address == person && !p.Remote
		posts = append(posts, postView{Post: p, Unread: seen && !mine, Parent: authors[p.ParentID]})
		if p.ID == cursor {
			seen = true
		}
	}
	if len(read.Posts) > 0 {
		_ = h.svc.MarkRead(r.Context(), board.MarkReadRequest{Actor: actor, Thread: read.Thread.ID, Post: read.Posts[len(read.Posts)-1].ID})
	}
	b, _ := h.svc.GetBoard(r.Context(), read.Thread.Board)
	h.render(w, "thread", page{Title: read.Thread.Title, Person: person, Data: map[string]any{"Thread": read.Thread, "Board": b, "Posts": posts, "More": read.Next}}, http.StatusOK)
}

// attach uploads a form's files and returns the Markdown that shows them.
func (h *Handler) attach(r *http.Request) (string, error) {
	if r.MultipartForm == nil {
		return "", nil
	}
	var lines []string
	for _, file := range r.MultipartForm.File["attach"] {
		f, err := file.Open()
		if err != nil {
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(f, 100<<20))
		f.Close()
		if err != nil {
			return "", err
		}
		a, err := h.svc.Upload(r.Context(), file.Filename, data)
		if err != nil {
			return "", fmt.Errorf("%s: %w", file.Filename, err)
		}
		alt := strings.TrimSuffix(file.Filename, filepathExt(file.Filename))
		lines = append(lines, fmt.Sprintf("![%s](blob:sha256-%s)", strings.NewReplacer("[", "", "]", "").Replace(alt), a.Ref))
	}
	return strings.Join(lines, "\n\n"), nil
}

func filepathExt(name string) string {
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[i:]
	}
	return ""
}

func (h *Handler) form(r *http.Request) error {
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/form-data") {
		return r.ParseMultipartForm(32 << 20)
	}
	return r.ParseForm()
}

func joinBody(text, shown string) string {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\r\n", "\n"))
	switch {
	case shown == "":
		return text
	case text == "":
		return shown
	}
	return text + "\n\n" + shown
}

func (h *Handler) open(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	if err := h.form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	shown, err := h.attach(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, _, err := h.svc.OpenThread(r.Context(), board.OpenThreadRequest{Actor: board.Actor{Person: person}, OperationID: r.FormValue("op"), Board: r.PathValue("board"), Title: r.FormValue("title"), Body: joinBody(r.FormValue("body"), shown), Kind: r.FormValue("kind"), Urgent: r.FormValue("urgent") != ""})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/t/"+out.Thread.ID, http.StatusSeeOther)
}

func (h *Handler) reply(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	if err := h.form(r); err != nil {
		h.fail(w, r, err)
		return
	}
	shown, err := h.attach(r)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	out, _, err := h.svc.Reply(r.Context(), board.ReplyRequest{Actor: board.Actor{Person: person}, OperationID: r.FormValue("op"), Thread: r.PathValue("thread"), Parent: r.FormValue("parent"), Body: joinBody(r.FormValue("body"), shown), Kind: r.FormValue("kind"), Urgent: r.FormValue("urgent") != ""})
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/t/"+out.Thread.ID+"#"+out.Post.ID, http.StatusSeeOther)
}

func (h *Handler) blob(w http.ResponseWriter, r *http.Request) {
	a, path, err := h.svc.BlobPath(r.Context(), r.PathValue("ref"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", a.Type)
	w.Header().Set("Cache-Control", "private, max-age=86400, immutable")
	http.ServeFile(w, r, path)
}

func (h *Handler) held(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	held, err := h.svc.Held(r.Context())
	if err != nil {
		h.fail(w, r, err)
		return
	}
	h.render(w, "held", page{Title: "Held media", Person: person, Data: held}, http.StatusOK)
}

func (h *Handler) release(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	request := board.HeldRequest{Actor: board.Actor{Person: person}, Ref: r.PathValue("ref")}
	var err error
	switch r.PathValue("op") {
	case "release":
		_, err = h.svc.Release(r.Context(), request)
	case "drop":
		_, err = h.svc.Drop(r.Context(), request)
	default:
		err = errors.New("release or drop")
	}
	if err != nil {
		h.fail(w, r, err)
		return
	}
	http.Redirect(w, r, "/held", http.StatusSeeOther)
}
