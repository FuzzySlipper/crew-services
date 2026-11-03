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
	"strconv"
	"strings"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/humansclient"
)

//go:embed static
var static embed.FS

const personCookie = "crew_board_person"

// Handler serves the pages.
type Handler struct {
	svc   *board.Service
	inbox Inbox
	tmpl  *template.Template
	mux   *http.ServeMux
}

// Inbox is a person's notices in crew-humans (docs/crew-humans.md), shown
// on the pages' notifications page and as the bell's count.
type Inbox interface {
	Notices(ctx context.Context, person string, unreadOnly bool, before string, limit int) (humansclient.Inbox, error)
	Notice(ctx context.Context, id string) (humansclient.Notice, error)
	MarkRead(ctx context.Context, person string, ids []string, through, group string) error
}

// Option configures the pages.
type Option func(*Handler)

// WithInbox shows each person's crew-humans notices.
func WithInbox(inbox Inbox) Option { return func(h *Handler) { h.inbox = inbox } }

// NewHandler serves crew-board's pages. Requests from outside private
// networks (the LAN, Tailscale's 100.64.0.0/10, loopback) are refused, and a
// form is accepted only from the page's own origin.
func NewHandler(svc *board.Service, opts ...Option) http.Handler {
	h := &Handler{svc: svc, mux: http.NewServeMux()}
	for _, opt := range opts {
		opt(h)
	}
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
	if h.inbox != nil {
		h.mux.HandleFunc("GET /notices", h.notices)
		h.mux.HandleFunc("GET /notices/{id}", h.openNotice)
		h.mux.HandleFunc("POST /notices/read", h.readNotices)
	}
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
	name := agentName(a)
	if a.WorkingFor != "" && a.Kind != board.AuthorPerson {
		name += " · for " + displayName(a.WorkingFor)
	}
	return name
}

func agentName(a board.Author) string {
	switch a.Kind {
	case board.AuthorPerson:
		return displayName(a.Address)
	case board.AuthorSeat:
		return a.Address + " · " + runtimeName(a.Adapter) + withModel(a.Label) + withModel(a.Model)
	case board.AuthorSession:
		if a.Label != "" {
			return a.Label + " · " + runtimeName(a.Adapter) + withModel(a.Model)
		}
		return runtimeName(a.Adapter) + withModel(a.Model) + " session " + a.Address
	}
	return a.Address
}

func withModel(model string) string {
	if model == "" {
		return ""
	}
	return " · " + model
}

func runtimeName(adapter string) string {
	switch adapter {
	case "claude-code":
		return "Claude Code"
	case "crew-codex":
		return "Codex"
	case "crew-hermes":
		return "Hermes"
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
	Nav    *navData
	Data   any
	Error  string
}

// navData is the left panel every page for a known person shows: the
// boards, and the open board's threads.
type navData struct {
	// Inbox says the bell is shown; Notices is its unread count.
	Inbox   bool
	Notices int
	Boards  []navBoard
	Board   string
	Threads []navThread
	More    bool
	Current string
}

type navBoard struct {
	Name    string
	Network string
	Unread  int
}

type navThread struct {
	board.Thread
	Unread   int
	Mentions int
}

// navThreads is how many of a board's threads the panel lists.
const navThreads = 60

// nav builds the left panel for person, with boardName's threads listed and
// thread marked as the one open. A failure leaves the panel out.
func (h *Handler) nav(ctx context.Context, person, boardName, thread string) *navData {
	boards, err := h.svc.ListBoards(ctx)
	if err != nil {
		return nil
	}
	unread := h.unread(ctx, person)
	perBoard := map[string]int{}
	for _, u := range unread {
		perBoard[u.Thread.Board] += u.Posts
	}
	nav := &navData{Board: boardName, Current: thread, Inbox: h.inbox != nil}
	if h.inbox != nil {
		if inbox, err := h.inbox.Notices(ctx, person, true, "", 1); err == nil {
			nav.Notices = inbox.Unread
		}
	}
	for _, b := range boards {
		nav.Boards = append(nav.Boards, navBoard{Name: b.Name, Network: b.Network, Unread: perBoard[b.Name]})
	}
	if boardName == "" {
		return nav
	}
	threads, err := h.svc.ListThreads(ctx, boardName, "", navThreads, nil, false)
	if err != nil {
		return nav
	}
	nav.More = threads.Next != ""
	for _, t := range threads.Threads {
		u := unread[t.ID]
		nav.Threads = append(nav.Threads, navThread{Thread: t, Unread: u.Posts, Mentions: u.Mentions})
	}
	return nav
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
	p := page{Title: "Something went wrong", Person: h.person(r), Error: err.Error()}
	if p.Person != "" {
		p.Nav = h.nav(r.Context(), p.Person, "", "")
	}
	h.render(w, "error", p, status)
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
	nav := h.nav(r.Context(), person, "", "")
	perBoard := map[string]int{}
	if nav != nil {
		for _, b := range nav.Boards {
			perBoard[b.Name] = b.Unread
		}
	}
	list, _ := h.svc.Unread(r.Context(), board.Actor{Person: person}, 50)
	h.render(w, "boards", page{Title: "Boards", Person: person, Nav: nav, Data: map[string]any{"Boards": boards, "Unread": perBoard, "New": list}}, http.StatusOK)
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
	h.render(w, "board", page{Title: b.Name, Person: person, Nav: h.nav(r.Context(), person, b.Name, ""), Data: map[string]any{"Board": b, "Threads": threads, "Unread": h.unread(r.Context(), person), "Archived": archived}}, http.StatusOK)
}

type postView struct {
	board.Post
	Unread     bool
	Parent     string
	ParentPage int
}

// threadPage is how many posts one thread page shows.
const threadPage = 100

func (h *Handler) thread(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	// The whole thread is read so any page can be shown; a page is a fixed
	// slice of it, so links stay stable as replies arrive.
	read, err := h.svc.ReadThread(r.Context(), r.PathValue("thread"), "", 200)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	all := read.Posts
	for next := read.Next; next != ""; {
		more, err := h.svc.ReadThread(r.Context(), read.Thread.ID, next, 200)
		if err != nil {
			h.fail(w, r, err)
			return
		}
		all, next = append(all, more.Posts...), more.Next
	}
	actor := board.Actor{Person: person}
	cursor, _ := h.svc.Cursor(r.Context(), actor, read.Thread.ID)
	// Posts come in the order written; everything after the cursor is new.
	unread := make([]bool, len(all))
	seen, firstUnread := cursor == "", -1
	for i, p := range all {
		mine := p.Author.Kind == board.AuthorPerson && p.Author.Address == person && !p.Remote
		unread[i] = seen && !mine
		if unread[i] && firstUnread < 0 {
			firstUnread = i
		}
		if p.ID == cursor {
			seen = true
		}
	}
	pages := max(1, (len(all)+threadPage-1)/threadPage)
	// With no page asked for, open where the reader's new posts start, or at
	// the newest page when nothing is new.
	number := pages
	if firstUnread >= 0 {
		number = firstUnread/threadPage + 1
	}
	if post := r.URL.Query().Get("post"); post != "" {
		if n := h.pageOf(all, post); n > 0 {
			number = n
		}
	}
	switch asked := r.URL.Query().Get("page"); asked {
	case "", "new":
	case "last":
		number = pages
	default:
		if n, err := strconv.Atoi(asked); err == nil && n >= 1 && n <= pages {
			number = n
		}
	}
	from, to := (number-1)*threadPage, min(number*threadPage, len(all))
	authors := map[string]string{}
	for _, p := range all {
		authors[p.ID] = authorName(p.Author)
	}
	posts := make([]postView, 0, to-from)
	for i := from; i < to; i++ {
		posts = append(posts, postView{Post: all[i], Unread: unread[i], Parent: authors[all[i].ParentID], ParentPage: h.pageOf(all, all[i].ParentID)})
	}
	// Reading a page marks it read; the cursor never moves back.
	if to > from {
		_ = h.svc.MarkRead(r.Context(), board.MarkReadRequest{Actor: actor, Thread: read.Thread.ID, Post: all[to-1].ID})
	}
	// Opening the thread is reading its notices too, so the bell does not
	// keep counting a post already read from a push.
	if h.inbox != nil {
		_ = h.inbox.MarkRead(r.Context(), person, nil, "", read.Thread.ID)
	}
	var numbers []int
	if pages > 1 {
		for n := 1; n <= pages; n++ {
			numbers = append(numbers, n)
		}
	}
	b, _ := h.svc.GetBoard(r.Context(), read.Thread.Board)
	h.render(w, "thread", page{Title: read.Thread.Title, Person: person, Nav: h.nav(r.Context(), person, read.Thread.Board, read.Thread.ID), Data: map[string]any{"Thread": read.Thread, "Board": b, "Posts": posts, "Page": number, "Pages": numbers, "Last": pages}}, http.StatusOK)
}

// pageOf is the thread page that shows post id, or 0.
func (h *Handler) pageOf(all []board.Post, id string) int {
	for i, p := range all {
		if p.ID == id {
			return i/threadPage + 1
		}
	}
	return 0
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
	http.Redirect(w, r, "/t/"+out.Thread.ID+"?page=last#"+out.Post.ID, http.StatusSeeOther)
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
	h.render(w, "held", page{Title: "Held media", Person: person, Nav: h.nav(r.Context(), person, "", ""), Data: held}, http.StatusOK)
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

// notices lists the person's notices in crew-humans, newest first.
func (h *Handler) notices(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	// Unread notices come first, all of them, so the bell's count can always
	// be found; then a page of everything, which pages back with ?before=.
	before := r.URL.Query().Get("before")
	var unread []humansclient.Notice
	if before == "" {
		for cursor := ""; ; {
			page, err := h.inbox.Notices(r.Context(), person, true, cursor, noticesPage)
			if err != nil {
				h.fail(w, r, fmt.Errorf("crew-humans: %w", err))
				return
			}
			unread = append(unread, page.Notices...)
			if len(page.Notices) < noticesPage || len(unread) >= maxUnreadShown {
				break
			}
			cursor = page.Notices[len(page.Notices)-1].ID
		}
	}
	inbox, err := h.inbox.Notices(r.Context(), person, false, before, noticesPage)
	if err != nil {
		h.fail(w, r, fmt.Errorf("crew-humans: %w", err))
		return
	}
	older := ""
	if len(inbox.Notices) == noticesPage {
		older = inbox.Notices[len(inbox.Notices)-1].ID
	}
	var earlier []humansclient.Notice
	for _, n := range inbox.Notices {
		if n.ReadAt != nil {
			earlier = append(earlier, n)
		}
	}
	newest := ""
	if len(unread) > 0 {
		newest = unread[0].ID
	}
	h.render(w, "notices", page{Title: "Notifications", Person: person, Nav: h.nav(r.Context(), person, "", ""), Data: map[string]any{"Unread": unread, "Read": earlier, "Newest": newest, "Older": older, "Paged": before != ""}}, http.StatusOK)
}

// openNotice marks one notice read and follows its link: to the post when
// it is on these pages, back to the list otherwise.
func (h *Handler) openNotice(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	target := "/notices"
	// Only the reader's own notice is opened; anyone else's is left alone.
	if n, err := h.inbox.Notice(r.Context(), r.PathValue("id")); err == nil && n.Person == person {
		_ = h.inbox.MarkRead(r.Context(), person, []string{n.ID}, "", "")
		if local := localLink(n.Link); local != "" {
			target = local
		}
	}
	http.Redirect(w, r, target, http.StatusSeeOther)
}

// noticesPage is how many notices the notifications page shows at a time;
// maxUnreadShown bounds the unread list above it.
const (
	noticesPage    = 100
	maxUnreadShown = 1000
)

// localLink is the path of a link to these pages (/t/...), or "".
func localLink(link string) string {
	u, err := url.Parse(link)
	if err != nil || !strings.HasPrefix(u.Path, "/t/") {
		return ""
	}
	out := u.Path
	if u.RawQuery != "" {
		out += "?" + u.RawQuery
	}
	if u.Fragment != "" {
		out += "#" + u.Fragment
	}
	return out
}

// readNotices marks every notice up to the newest shown read.
func (h *Handler) readNotices(w http.ResponseWriter, r *http.Request) {
	person, ok := h.needPerson(w, r)
	if !ok {
		return
	}
	if through := r.FormValue("through"); through != "" {
		if err := h.inbox.MarkRead(r.Context(), person, nil, through, ""); err != nil {
			h.fail(w, r, fmt.Errorf("crew-humans: %w", err))
			return
		}
	}
	http.Redirect(w, r, "/notices", http.StatusSeeOther)
}
