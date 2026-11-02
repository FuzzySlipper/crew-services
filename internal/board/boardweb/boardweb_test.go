package boardweb_test

import (
	"bytes"
	"context"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/board/boardsqlite"
	"crew-services/internal/board/boardweb"
)

// nobody is a directory with no sessions or seats: these pages are for
// people.
type nobody struct{}

func (nobody) Session(context.Context, string) (string, bool, error)    { return "", false, nil }
func (nobody) SeatHolder(context.Context, string) (string, bool, error) { return "", false, nil }

type web struct {
	t       *testing.T
	ctx     context.Context
	dir     string
	svc     *board.Service
	handler http.Handler
	ops     int
}

func newWeb(t *testing.T) *web {
	t.Helper()
	dir := t.TempDir()
	store, err := boardsqlite.Open(context.Background(), filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := board.New(store, nobody{}, time.Now, board.Config{Site: "den", People: []string{"patch", "lore"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateBoard(context.Background(), board.CreateBoardRequest{Actor: board.Actor{Person: "patch"}, OperationID: "make", Name: "design", Description: "design talk"}); err != nil {
		t.Fatal(err)
	}
	return &web{t: t, ctx: context.Background(), dir: dir, svc: svc, handler: boardweb.NewHandler(svc)}
}

// do sends a request as person from a LAN address; post forms carry the
// page's own origin.
func (w *web) do(method, path, person string, form url.Values) *httptest.ResponseRecorder {
	w.t.Helper()
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	r := httptest.NewRequest(method, "http://192.168.1.10:8795"+path, body)
	r.RemoteAddr = "192.168.1.20:51000"
	if form != nil {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("Origin", "http://192.168.1.10:8795")
	}
	if person != "" {
		r.AddCookie(&http.Cookie{Name: "crew_board_person", Value: person})
	}
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, r)
	return rec
}

func TestRenderingAllowsOnlyDetailsAndSummary(t *testing.T) {
	body := "Hello <b>bold</b> <script>alert(1)</script> <img src=x onerror=alert(2)> <iframe src=\"https://evil.example\"></iframe>\n\n" +
		"[click](javascript:alert(3)) and **real bold**\n\n" +
		"<details open><summary>Bench<script>x</script> results</summary>\n\n| a | b |\n|---|---|\n| 1 | 2 |\n\n<i>raw</i> inside\n</details>\n\n" +
		"![remote](https://example.com/tracker.png)"
	html := string(boardweb.RenderPost(body, nil))
	for _, banned := range []string{"<script", "<iframe", "javascript:", "<b>", "<i>", "<img"} {
		if strings.Contains(html, banned) {
			t.Errorf("rendered HTML has %q:\n%s", banned, html)
		}
	}
	for _, want := range []string{"<details open><summary>Bench&lt;script&gt;x&lt;/script&gt; results</summary>", "<table>", "<strong>real bold</strong>", `<a href="https://example.com/tracker.png" rel="noopener noreferrer nofollow">image: remote</a>`} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML lacks %q:\n%s", want, html)
		}
	}
	if regexp.MustCompile(`<[^>]*onerror`).MatchString(html) {
		t.Errorf("a live tag carries onerror:\n%s", html)
	}
	// Every HTML tag is one goldmark makes, or details/summary.
	allowed := map[string]bool{"p": true, "a": true, "strong": true, "em": true, "del": true, "code": true, "pre": true, "table": true, "thead": true, "tbody": true, "tr": true, "th": true, "td": true, "details": true, "summary": true, "span": true, "br": true, "ul": true, "ol": true, "li": true, "blockquote": true, "h1": true, "h2": true, "h3": true, "hr": true, "input": true}
	for _, m := range regexp.MustCompile(`<([a-zA-Z0-9]+)`).FindAllStringSubmatch(html, -1) {
		if !allowed[strings.ToLower(m[1])] {
			t.Errorf("unexpected tag <%s> in:\n%s", m[1], html)
		}
	}
}

func TestPagesFetchNothingFromElsewhere(t *testing.T) {
	w := newWeb(t)
	opened, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: "open", Board: "design", Title: "Remote things", Body: "![logo](https://cdn.example.com/logo.png)\n\n```mermaid\ngraph TD; A-->B\n```"})
	if err != nil {
		t.Fatal(err)
	}
	rec := w.do("GET", "/t/"+opened.Thread.ID, "lore", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("thread page = %d %s", rec.Code, rec.Body)
	}
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "img-src 'self'") || !strings.Contains(csp, "script-src 'self'") {
		t.Fatalf("CSP = %q", csp)
	}
	page := rec.Body.String()
	for _, m := range regexp.MustCompile(`(?i)<(img|script|link|video|iframe|source)[^>]*\s(src|href)="([^"]+)"`).FindAllStringSubmatch(page, -1) {
		if !strings.HasPrefix(m[3], "/") {
			t.Errorf("the page loads %s from %s", m[1], m[3])
		}
	}
	if !strings.Contains(page, `<script src="/static/mermaid.min.js"></script>`) || !strings.Contains(page, `class="language-mermaid"`) {
		t.Errorf("mermaid is not served locally:\n%s", page)
	}
	if css := w.do("GET", "/static/style.css", "", nil); css.Code != http.StatusOK || !strings.Contains(css.Body.String(), "color-scheme: dark") {
		t.Errorf("stylesheet = %d", css.Code)
	}
}

func TestFormsWriteWhatTheCLIWrites(t *testing.T) {
	w := newWeb(t)
	rec := w.do("POST", "/b/design/new", "patch", url.Values{"op": {"web-1"}, "title": {"From the page"}, "body": {"Hello @lore, a question."}, "kind": {"question"}})
	if rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/t/den-") {
		t.Fatalf("new thread = %d %s", rec.Code, rec.Body)
	}
	threadID := strings.TrimPrefix(rec.Header().Get("Location"), "/t/")
	viaCLI, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: "cli-1", Board: "design", Title: "From the CLI", Body: "Hello @lore, a question.", Kind: "question"})
	if err != nil {
		t.Fatal(err)
	}
	fromPage, _ := w.svc.ReadThread(w.ctx, threadID, "", 10)
	fromCLI, _ := w.svc.ReadThread(w.ctx, viaCLI.Thread.ID, "", 10)
	a, b := fromPage.Posts[0], fromCLI.Posts[0]
	if a.Author != b.Author || a.Body != b.Body || a.Kind != b.Kind || len(a.Mentions) != 1 || a.Mentions[0] != b.Mentions[0] || a.State != b.State {
		t.Fatalf("page record %+v\ndiffers from CLI record %+v", a, b)
	}
	notices, _ := w.svc.Notices(w.ctx, "", 10)
	if len(notices) != 2 || notices[0].Recipient != notices[1].Recipient || notices[0].Reason != notices[1].Reason {
		t.Fatalf("notices = %+v", notices)
	}
	// The same form sent twice (a double click) writes once.
	w.do("POST", "/b/design/new", "patch", url.Values{"op": {"web-1"}, "title": {"From the page"}, "body": {"Hello @lore, a question."}, "kind": {"question"}})
	if page, _ := w.svc.ListThreads(w.ctx, "design", "", 10, nil, false); len(page.Threads) != 2 {
		t.Fatalf("threads after a double submit = %d", len(page.Threads))
	}
	// A reply form, then lore sees it marked new until she has read it.
	if rec := w.do("POST", "/t/"+threadID+"/reply", "patch", url.Values{"op": {"web-2"}, "parent": {a.ID}, "body": {"Following up."}}); rec.Code != http.StatusSeeOther {
		t.Fatalf("reply = %d %s", rec.Code, rec.Body)
	}
	if board := w.do("GET", "/b/design", "lore", nil).Body.String(); !strings.Contains(board, "new</span>") || !strings.Contains(board, "mentions you") {
		t.Fatalf("board page for lore lacks unread markers:\n%s", board)
	}
	first := w.do("GET", "/t/"+threadID, "lore", nil).Body.String()
	if strings.Count(first, `class="post unread`) != 2 {
		t.Fatalf("lore's first view should mark both posts new:\n%s", first)
	}
	if again := w.do("GET", "/t/"+threadID, "lore", nil).Body.String(); strings.Contains(again, `class="post unread`) {
		t.Fatal("posts stay new after lore read them")
	}
}

func TestOnlyPrivateNetworksAndOwnPagesGetIn(t *testing.T) {
	w := newWeb(t)
	for addr, want := range map[string]int{"8.8.8.8:5000": http.StatusForbidden, "203.0.113.9:1": http.StatusForbidden, "100.101.102.103:4000": http.StatusSeeOther, "10.0.0.7:9": http.StatusSeeOther, "127.0.0.1:2": http.StatusSeeOther, "[fd7a:115c:a1e0::1]:3": http.StatusSeeOther} {
		r := httptest.NewRequest("GET", "http://board/", nil)
		r.RemoteAddr = addr
		rec := httptest.NewRecorder()
		w.handler.ServeHTTP(rec, r)
		if rec.Code != want {
			t.Errorf("GET from %s = %d, want %d", addr, rec.Code, want)
		}
	}
	post := func(origin string, fetchSite ...string) int {
		r := httptest.NewRequest("POST", "http://192.168.1.10:8795/b/design/new", strings.NewReader("title=x&body=y&op=z"))
		r.RemoteAddr = "192.168.1.20:1"
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		if len(fetchSite) > 0 {
			r.Header.Set("Sec-Fetch-Site", fetchSite[0])
		}
		r.AddCookie(&http.Cookie{Name: "crew_board_person", Value: "patch"})
		rec := httptest.NewRecorder()
		w.handler.ServeHTTP(rec, r)
		return rec.Code
	}
	if code := post(""); code != http.StatusForbidden {
		t.Errorf("a form with no origin = %d", code)
	}
	if code := post("http://evil.example"); code != http.StatusForbidden {
		t.Errorf("a form from another site = %d", code)
	}
	if code := post("http://192.168.1.10:8795"); code != http.StatusSeeOther {
		t.Errorf("a form from this board = %d", code)
	}
	if code := post("", "same-origin"); code != http.StatusSeeOther {
		t.Errorf("a browser's own form with no origin header = %d", code)
	}
	if code := post("http://192.168.1.10:8795", "cross-site"); code != http.StatusForbidden {
		t.Errorf("a cross-site form = %d", code)
	}
	// With no-referrer a browser sends "Origin: null" on these pages' own
	// forms, so the policy must let it name this origin.
	if policy := w.do("GET", "/who", "", nil).Header().Get("Referrer-Policy"); policy != "same-origin" {
		t.Errorf("Referrer-Policy = %q", policy)
	}
	if rec := w.do("GET", "/", "mallory", nil); rec.Code != http.StatusSeeOther || !strings.HasPrefix(rec.Header().Get("Location"), "/who") {
		t.Errorf("an unknown person is sent to choose: %d %s", rec.Code, rec.Header().Get("Location"))
	}
}

func TestAnImageAttachedThroughTheForm(t *testing.T) {
	for _, tool := range []string{"ffmpeg", "ffprobe", "bwrap", "prlimit"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s is not installed", tool)
		}
	}
	w := newWeb(t)
	in, err := board.NewIngester(board.DefaultMediaConfig(filepath.Join(w.dir, "media")), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	store, _ := boardsqlite.Open(w.ctx, filepath.Join(w.dir, "board.db"))
	defer store.Close()
	w.svc.UseMedia(in, store)
	image := filepath.Join(t.TempDir(), "sketch.png")
	if out, err := exec.Command("ffmpeg", "-hide_banner", "-loglevel", "error", "-f", "lavfi", "-i", "testsrc=size=80x60", "-frames:v", "1", image).CombinedOutput(); err != nil {
		t.Fatalf("ffmpeg: %v %s", err, out)
	}
	data, _ := os.ReadFile(image)
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.WriteField("op", "web-img")
	_ = mw.WriteField("title", "A sketch")
	_ = mw.WriteField("body", "Here it is.")
	part, _ := mw.CreateFormFile("attach", "sketch.png")
	_, _ = part.Write(data)
	_ = mw.Close()
	r := httptest.NewRequest("POST", "http://192.168.1.10:8795/b/design/new", &body)
	r.RemoteAddr = "192.168.1.20:1"
	r.Header.Set("Content-Type", mw.FormDataContentType())
	r.Header.Set("Origin", "http://192.168.1.10:8795")
	r.AddCookie(&http.Cookie{Name: "crew_board_person", Value: "lore"})
	rec := httptest.NewRecorder()
	w.handler.ServeHTTP(rec, r)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("form with an image = %d %s", rec.Code, rec.Body)
	}
	page := w.do("GET", rec.Header().Get("Location"), "patch", nil).Body.String()
	m := regexp.MustCompile(`<img src="/blob/([0-9a-f]{64})" alt="sketch"`).FindStringSubmatch(page)
	if m == nil {
		t.Fatalf("the page does not show the image:\n%s", page)
	}
	if blob := w.do("GET", "/blob/"+m[1], "patch", nil); blob.Code != http.StatusOK || blob.Header().Get("Content-Type") != "image/webp" {
		t.Fatalf("blob = %d %s", blob.Code, blob.Header().Get("Content-Type"))
	}
}

func TestFoldsQuotedInCodeStayCode(t *testing.T) {
	body := "Write a fold like this:\n\n```html\n<details><summary>Example</summary>text</details>\n```\n\n" +
		"or inline, `<details><summary>Inline</summary>x</details>`.\n\n" +
		"<details><summary>Real</summary>\n\nfolded **text**\n\n</details>\n\n" +
		"<details><summary>Never closed</summary>\n\nstill inside"
	html := string(boardweb.RenderPost(body, nil))
	if n := strings.Count(html, "<details>"); n != 2 {
		t.Fatalf("want the real fold and the unclosed one live, got %d:\n%s", n, html)
	}
	if strings.Count(html, "</details>") != 2 {
		t.Fatalf("folds are not balanced:\n%s", html)
	}
	for _, want := range []string{
		`<pre><code class="language-html">&lt;details&gt;&lt;summary&gt;Example&lt;/summary&gt;text&lt;/details&gt;`,
		`<code>&lt;details&gt;&lt;summary&gt;Inline&lt;/summary&gt;x&lt;/details&gt;</code>`,
		"<details><summary>Real</summary>",
		"<strong>text</strong>",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("rendered HTML lacks %q:\n%s", want, html)
		}
	}
}

func TestLongThreadsPage(t *testing.T) {
	w := newWeb(t)
	opened, _, err := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: "open", Board: "design", Title: "Long", Body: "post 0"})
	if err != nil {
		t.Fatal(err)
	}
	thread := opened.Thread.ID
	for i := 1; i <= 230; i++ {
		if _, _, err := w.svc.Reply(w.ctx, board.ReplyRequest{Actor: board.Actor{Person: "patch"}, OperationID: fmt.Sprintf("r%d", i), Thread: thread, Body: fmt.Sprintf("post %d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	shows := func(page, path string, posts ...int) {
		t.Helper()
		for _, n := range posts {
			if !strings.Contains(page, fmt.Sprintf("<p>post %d</p>", n)) {
				t.Errorf("%s does not show post %d", path, n)
			}
		}
	}
	// Lore has read nothing, so the page opens at the start, and each page
	// she reads is marked read.
	first := w.do("GET", "/t/"+thread, "lore", nil).Body.String()
	shows(first, "page 1", 0, 99)
	if strings.Contains(first, "<p>post 100</p>") || !strings.Contains(first, `<a href="?page=3">3</a>`) {
		t.Fatalf("page 1 should hold posts 0-99 and link page 3")
	}
	// Coming back, she opens where her new posts start.
	second := w.do("GET", "/t/"+thread, "lore", nil).Body.String()
	shows(second, "the next visit", 100, 199)
	third := w.do("GET", "/t/"+thread+"?page=3", "lore", nil).Body.String()
	shows(third, "page 3", 200, 230)
	if again := w.do("GET", "/t/"+thread+"?page=1", "lore", nil).Body.String(); strings.Contains(again, `class="post unread`) {
		t.Fatal("going back to page 1 marks posts new again")
	}
	if unread, _ := w.svc.Unread(w.ctx, board.Actor{Person: "lore"}, 10); len(unread) != 0 {
		t.Fatalf("lore read every page and still has %+v", unread)
	}
	// A reply lands on the last page, where it shows.
	rec := w.do("POST", "/t/"+thread+"/reply", "lore", url.Values{"op": {"lore-1"}, "body": {"the newest reply"}})
	where := rec.Header().Get("Location")
	if !strings.Contains(where, "?page=last#") {
		t.Fatalf("reply went to %q", where)
	}
	shows(w.do("GET", strings.Split(where, "#")[0], "lore", nil).Body.String(), "the last page", 230)
	if !strings.Contains(w.do("GET", strings.Split(where, "#")[0], "lore", nil).Body.String(), "the newest reply") {
		t.Fatal("the last page does not show the reply just posted")
	}
}

func TestEveryPageHasTheBoardPanel(t *testing.T) {
	w := newWeb(t)
	opened, _, _ := w.svc.OpenThread(w.ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "patch"}, OperationID: "open", Board: "design", Title: "Panel check", Body: "hi @lore"})
	thread := w.do("GET", "/t/"+opened.Thread.ID, "patch", nil).Body.String()
	if !strings.Contains(thread, `<aside class="nav">`) || !strings.Contains(thread, `<li class="current">`) || !strings.Contains(thread, "Panel check") {
		t.Fatalf("thread page lacks the panel with its thread current:\n%s", thread)
	}
	home := w.do("GET", "/", "lore", nil).Body.String()
	if !strings.Contains(home, `<span class="count">1</span>`) || !strings.Contains(home, "mentions you") {
		t.Fatalf("lore's home lacks her unread count and mention:\n%s", home)
	}
}
