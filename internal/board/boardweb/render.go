package boardweb

import (
	"bytes"
	"html"
	"html/template"
	"io"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer"
	ghtml "github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/util"

	"crew-services/internal/board"
)

// foldTag is a tag a post may use: <details>, <details open>, <summary> and
// their closing tags.
var foldTag = regexp.MustCompile(`(?i)<(/?)(details|summary)(\s+open)?\s*>`)

// RenderPost turns a post's Markdown into HTML that is safe to show. Folds
// are found where Markdown itself finds raw HTML, so a fold quoted in code
// stays code. There, <details>/<summary> are rebuilt balanced, and any other
// tag shows as text. Links with dangerous schemes are blanked; images become
// this board's own <img> or <video> for attachments, and links for anything
// remote, so a page never fetches from elsewhere.
func RenderPost(body string, attachments []board.Attachment) template.HTML {
	byRef := map[string]board.Attachment{}
	for _, a := range attachments {
		byRef[a.Ref] = a
	}
	if strings.TrimSpace(body) == "" {
		return ""
	}
	folds := &foldRenderer{}
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
		goldmark.WithRendererOptions(ghtml.WithHardWraps(), renderer.WithNodeRenderers(
			util.Prioritized(&imageRenderer{byRef: byRef}, 100),
			util.Prioritized(folds, 100),
		)),
	)
	var out bytes.Buffer
	if err := md.Convert([]byte(body), &out); err != nil {
		return template.HTML("<p>" + html.EscapeString(body) + "</p>")
	}
	folds.closeAll(&out)
	return template.HTML(out.String())
}

// foldRenderer draws raw HTML, block and inline: fold tags pass, rebuilt
// and kept balanced across the post; everything else is escaped as text.
type foldRenderer struct {
	open      int
	inSummary bool
}

func (r *foldRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindHTMLBlock, r.block)
	reg.Register(ast.KindRawHTML, r.inline)
}

func (r *foldRenderer) block(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.HTMLBlock)
	var text bytes.Buffer
	for i := 0; i < n.Lines().Len(); i++ {
		line := n.Lines().At(i)
		text.Write(line.Value(source))
	}
	if n.HasClosure() {
		text.Write(n.ClosureLine.Value(source))
	}
	r.write(w, text.String())
	return ast.WalkSkipChildren, nil
}

func (r *foldRenderer) inline(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	n := node.(*ast.RawHTML)
	var text bytes.Buffer
	for i := 0; i < n.Segments.Len(); i++ {
		segment := n.Segments.At(i)
		text.Write(segment.Value(source))
	}
	r.write(w, text.String())
	return ast.WalkSkipChildren, nil
}

func (r *foldRenderer) write(w io.Writer, text string) {
	last := 0
	for _, loc := range foldTag.FindAllStringSubmatchIndex(text, -1) {
		_, _ = io.WriteString(w, html.EscapeString(text[last:loc[0]]))
		last = loc[1]
		closing, name, open := loc[2] < loc[3], strings.ToLower(text[loc[4]:loc[5]]), loc[6] >= 0
		switch {
		case name == "details" && !closing:
			r.endSummary(w)
			r.open++
			if open {
				_, _ = io.WriteString(w, "<details open>")
			} else {
				_, _ = io.WriteString(w, "<details>")
			}
		case name == "details" && r.open > 0:
			r.endSummary(w)
			r.open--
			_, _ = io.WriteString(w, "</details>")
		case name == "summary" && !closing && r.open > 0 && !r.inSummary:
			r.inSummary = true
			_, _ = io.WriteString(w, "<summary>")
		case name == "summary" && closing:
			r.endSummary(w)
		}
	}
	_, _ = io.WriteString(w, html.EscapeString(text[last:]))
}

func (r *foldRenderer) endSummary(w io.Writer) {
	if r.inSummary {
		r.inSummary = false
		_, _ = io.WriteString(w, "</summary>")
	}
}

// closeAll closes what the post left open.
func (r *foldRenderer) closeAll(w io.Writer) {
	r.endSummary(w)
	for ; r.open > 0; r.open-- {
		_, _ = io.WriteString(w, "</details>")
	}
}

// imageRenderer draws Markdown images: an attachment by its blob reference,
// with the title as its caption; anything else as a plain link.
type imageRenderer struct {
	byRef map[string]board.Attachment
}

func (r *imageRenderer) RegisterFuncs(reg renderer.NodeRendererFuncRegisterer) {
	reg.Register(ast.KindImage, r.image)
}

func altText(n ast.Node, source []byte) string {
	var b strings.Builder
	_ = ast.Walk(n, func(child ast.Node, entering bool) (ast.WalkStatus, error) {
		if t, ok := child.(*ast.Text); ok && entering {
			b.Write(t.Segment.Value(source))
		}
		return ast.WalkContinue, nil
	})
	return b.String()
}

func (r *imageRenderer) image(w util.BufWriter, source []byte, node ast.Node, entering bool) (ast.WalkStatus, error) {
	if !entering {
		return ast.WalkContinue, nil
	}
	img := node.(*ast.Image)
	dest, title, alt := string(img.Destination), string(img.Title), altText(node, source)
	ref, isBlob := strings.CutPrefix(dest, "blob:sha256-")
	if !isBlob {
		if strings.HasPrefix(dest, "https://") || strings.HasPrefix(dest, "http://") {
			_, _ = w.WriteString(`<a href="` + html.EscapeString(dest) + `" rel="noopener noreferrer nofollow">image: ` + html.EscapeString(alt) + `</a>`)
		} else {
			_, _ = w.WriteString(`<span class="held">image: ` + html.EscapeString(alt) + `</span>`)
		}
		return ast.WalkSkipChildren, nil
	}
	a, known := r.byRef[ref]
	switch {
	case !known || a.State == board.AttachmentDropped:
		_, _ = w.WriteString(`<span class="held">attachment removed</span>`)
	case a.State == board.AttachmentHeld:
		_, _ = w.WriteString(`<span class="held">attachment held: ` + html.EscapeString(a.Reason) + ` · <a href="/held">held media</a></span>`)
	default:
		src := "/blob/" + html.EscapeString(ref)
		_, _ = w.WriteString(`<span class="fig">`)
		if a.Kind == board.MediaMotion {
			_, _ = w.WriteString(`<video src="` + src + `" muted loop autoplay playsinline controls preload="metadata" title="` + html.EscapeString(alt) + `"></video>`)
		} else {
			_, _ = w.WriteString(`<img src="` + src + `" alt="` + html.EscapeString(alt) + `" loading="lazy">`)
		}
		if title != "" {
			_, _ = w.WriteString(`<span class="cap">` + html.EscapeString(title) + `</span>`)
		}
		_, _ = w.WriteString(`</span>`)
	}
	return ast.WalkSkipChildren, nil
}
