package board

import (
	"regexp"
	"strings"
)

// Mention is one participant a post names, resolved when the post was
// written: Kind is seat, session or person, Address is the participant on
// Site, and Text is how the post wrote it.
type Mention struct {
	Text    string `json:"text"`
	Kind    string `json:"kind"`
	Address string `json:"address"`
	Site    string `json:"site"`
}

// MentionWarning is a mention the author's site could not resolve. The post
// is still written; the reference stays plain text.
type MentionWarning struct {
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

// mentionPattern matches @name, @scope.name, @s/<hash> and any of those
// followed by @site, in the fediverse's @name@server form. The character
// before @ must not continue a word, an address or an email address.
var mentionPattern = regexp.MustCompile(`(^|[^A-Za-z0-9_@./-])@((?:s/[0-9a-f]+)|(?:[a-z0-9][a-z0-9-]*(?:\.[a-z0-9][a-z0-9-]*)?))(?:@([a-z0-9][a-z0-9-]*))?`)

// codePattern finds fenced blocks and inline code, whose @ is never a mention.
var codePattern = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")

// mentionCandidate is a reference before resolution.
type mentionCandidate struct {
	Text, Name, Site string
}

// mentionCandidates lists the distinct references in body, outside code, in
// the order they first appear.
func mentionCandidates(body string) []mentionCandidate {
	plain := codePattern.ReplaceAllStringFunc(body, func(code string) string { return strings.Repeat(" ", len(code)) })
	seen := map[string]bool{}
	var out []mentionCandidate
	for _, m := range mentionPattern.FindAllStringSubmatch(plain, -1) {
		name, site := m[2], m[3]
		// A trailing dot ends a sentence, not a seat name.
		name = strings.TrimSuffix(name, ".")
		text := "@" + name
		if site != "" {
			text += "@" + site
		}
		if seen[text] {
			continue
		}
		seen[text] = true
		out = append(out, mentionCandidate{Text: text, Name: name, Site: site})
	}
	return out
}

// participantKind says which kind of participant a bare name is by its form:
// s/<hash> is a session, scope.name a seat, and anything else a person.
func participantKind(name string) string {
	switch {
	case strings.HasPrefix(name, "s/"):
		return AuthorSession
	case strings.Contains(name, "."):
		return AuthorSeat
	default:
		return AuthorPerson
	}
}
