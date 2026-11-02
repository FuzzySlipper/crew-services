package boardweb

import (
	"bytes"
	"html"
	"html/template"
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

// detailsBlock is a folded section, the one piece of HTML a post may use.
var detailsBlock = regexp.MustCompile(`(?is)<details(\s+open)?\s*>\s*(?:<summary>(.*?)</summary>)?(.*?)</details>`)

// RenderPost turns a post's Markdown into HTML that is safe to show: raw HTML
// is dropped (goldmark omits it), except <details>/<summary>, which this
// renderer rebuilds itself with the summary escaped; links with dangerous
// schemes are blanked; images become this board's own <img> or <video> for
// attachments, and links for anything remote, so a page never fetches from
// elsewhere.
func RenderPost(body string, attachments []board.Attachment) template.HTML {
	byRef := map[string]board.Attachment{}
	for _, a := range attachments {
		byRef[a.Ref] = a
	}
	var out bytes.Buffer
	last := 0
	for _, loc := range detailsBlock.FindAllStringSubmatchIndex(body, -1) {
		out.WriteString(markdown(body[last:loc[0]], byRef))
		open := loc[2] >= 0
		summary := "Details"
		if loc[4] >= 0 {
			summary = strings.TrimSpace(body[loc[4]:loc[5]])
		}
		inner := body[loc[6]:loc[7]]
		if open {
			out.WriteString("<details open>")
		} else {
			out.WriteString("<details>")
		}
		out.WriteString("<summary>" + html.EscapeString(summary) + "</summary>")
		out.WriteString(markdown(inner, byRef))
		out.WriteString("</details>")
		last = loc[1]
	}
	out.WriteString(markdown(body[last:], byRef))
	return template.HTML(out.String())
}

func markdown(source string, byRef map[string]board.Attachment) string {
	if strings.TrimSpace(source) == "" {
		return ""
	}
	md := goldmark.New(
		goldmark.WithExtensions(extension.Table, extension.Strikethrough, extension.TaskList),
		goldmark.WithRendererOptions(ghtml.WithHardWraps(), renderer.WithNodeRenderers(util.Prioritized(&imageRenderer{byRef: byRef}, 100))),
	)
	var out bytes.Buffer
	if err := md.Convert([]byte(source), &out); err != nil {
		return "<p>" + html.EscapeString(source) + "</p>"
	}
	return out.String()
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
