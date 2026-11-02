package board

import (
	"fmt"
	"regexp"
	"strings"
)

// detailsPattern is a folded section: <details> with an optional <summary>.
var detailsPattern = regexp.MustCompile(`(?is)<details(\s+open)?\s*>\s*(?:<summary>(.*?)</summary>)?(.*?)</details>`)

// imagePattern is a Markdown image, ![alt](target "caption").
var imagePattern = regexp.MustCompile(`!\[([^\]]*)\]\(\s*([^\s)]+)(?:\s+"([^"]*)")?\s*\)`)

// RenderForAgent turns a post body into text meant for a model: each folded
// section becomes one line naming its summary and length, unless unfold is
// set, and each image becomes a one-line description. The Markdown itself is
// otherwise left as written.
func RenderForAgent(body string, unfold bool) string {
	out := detailsPattern.ReplaceAllStringFunc(body, func(section string) string {
		m := detailsPattern.FindStringSubmatch(section)
		summary := strings.TrimSpace(m[2])
		if summary == "" {
			summary = "details"
		}
		inner := strings.Trim(m[3], "\n")
		if unfold {
			return fmt.Sprintf("[%s]\n%s\n[end of %s]", summary, inner, summary)
		}
		lines := 0
		if strings.TrimSpace(inner) != "" {
			lines = strings.Count(inner, "\n") + 1
		}
		return fmt.Sprintf("[folded: %s, %d %s; --unfold to show]", summary, lines, plural(lines, "line", "lines"))
	})
	return imagePattern.ReplaceAllStringFunc(out, func(image string) string {
		m := imagePattern.FindStringSubmatch(image)
		alt, target, caption := strings.TrimSpace(m[1]), m[2], strings.TrimSpace(m[3])
		parts := []string{"image"}
		if alt != "" {
			parts[0] += ": " + alt
		}
		if caption != "" {
			parts = append(parts, "caption "+caption)
		}
		if hash, ok := strings.CutPrefix(target, "blob:"); ok {
			parts = append(parts, "board blob "+strings.TrimPrefix(hash, "sha256-"))
		} else {
			parts = append(parts, "link "+target)
		}
		return "[" + strings.Join(parts, "; ") + "]"
	})
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
