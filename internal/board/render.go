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
	return RenderForAgentWith(body, unfold, nil)
}

// RenderForAgentWith is RenderForAgent, describing each attachment the post
// names from what is known about it: its kind, size and state.
func RenderForAgentWith(body string, unfold bool, attachments []Attachment) string {
	byRef := map[string]Attachment{}
	for _, a := range attachments {
		byRef[a.Ref] = a
	}
	out := replaceFolds(body, func(m []string) string {
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
			ref := strings.TrimPrefix(hash, "sha256-")
			if a, known := byRef[ref]; known {
				switch a.State {
				case AttachmentHeld:
					return "[attachment held: " + a.Reason + "; a person on this site can release it with board release " + ref + "]"
				case AttachmentDropped:
					return "[attachment removed]"
				}
				if a.Kind == MediaMotion {
					parts[0] = "moving " + parts[0]
					parts = append(parts, fmt.Sprintf("%d×%d, %.0f s", a.Width, a.Height, float64(a.DurationMS)/1000))
				} else {
					parts = append(parts, fmt.Sprintf("%d×%d", a.Width, a.Height))
				}
			}
			parts = append(parts, "board blob "+ref)
		} else {
			parts = append(parts, "link "+target)
		}
		return "[" + strings.Join(parts, "; ") + "]"
	})
}

// replaceFolds replaces each folded section outside code with fold(match),
// where match holds the section and detailsPattern's groups. A fold quoted in
// a fenced block or an inline code span is left as written.
func replaceFolds(body string, fold func(match []string) string) string {
	masked := []byte(body)
	for _, r := range codeRanges(body) {
		for i := r[0]; i < r[1]; i++ {
			if masked[i] == '<' {
				masked[i] = 0
			}
		}
	}
	var out strings.Builder
	last := 0
	for _, loc := range detailsPattern.FindAllSubmatchIndex(masked, -1) {
		match := make([]string, len(loc)/2)
		for g := range match {
			if loc[2*g] >= 0 {
				match[g] = body[loc[2*g]:loc[2*g+1]]
			}
		}
		out.WriteString(body[last:loc[0]])
		out.WriteString(fold(match))
		last = loc[1]
	}
	out.WriteString(body[last:])
	return out.String()
}

// fencePattern opens or closes a fenced code block.
var fencePattern = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")

// codeRanges are the byte ranges of a body's fenced code blocks and inline
// code spans.
func codeRanges(body string) [][2]int {
	var ranges [][2]int
	fence, fenceStart := "", 0
	offset := 0
	var prose strings.Builder
	var proseAt []int
	for _, line := range strings.SplitAfter(body, "\n") {
		m := fencePattern.FindStringSubmatch(line)
		switch {
		case fence == "" && m != nil:
			fence, fenceStart = m[1], offset
		case fence != "" && m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) && strings.TrimSpace(line[len(m[0]):]) == "":
			ranges = append(ranges, [2]int{fenceStart, offset + len(line)})
			fence = ""
		case fence == "":
			for i := range len(line) {
				proseAt = append(proseAt, offset+i)
			}
			prose.WriteString(line)
		}
		offset += len(line)
	}
	if fence != "" {
		ranges = append(ranges, [2]int{fenceStart, len(body)})
	}
	// Inline spans: a run of backticks up to the next run of the same length.
	text := prose.String()
	for i := 0; i < len(text); {
		if text[i] != '`' {
			i++
			continue
		}
		run := i
		for run < len(text) && text[run] == '`' {
			run++
		}
		width, closed := run-i, false
		for j := run; j < len(text); {
			if text[j] != '`' {
				j++
				continue
			}
			k := j
			for k < len(text) && text[k] == '`' {
				k++
			}
			if k-j == width {
				ranges = append(ranges, [2]int{proseAt[i], proseAt[k-1] + 1})
				i, closed = k, true
				break
			}
			j = k
		}
		if !closed {
			i = run
		}
	}
	return ranges
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
