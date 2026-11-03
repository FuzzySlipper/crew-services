package crewcli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// A session's person is who it works for (docs/crew-humans.md, Sessions and
// their person). Each computer belongs to one person, so the default is the
// machine account's: CREW_PERSON, or ~/.config/crew/person. A session started
// on someone else's computer says otherwise with `whoami --person NAME`,
// which is kept per native session beside the default. The person is display
// information for posts and notices; the fabric never sees it.

var personRule = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

func personHome() string {
	home, _ := os.UserHomeDir()
	return home
}

func defaultPersonFile() string {
	return filepath.Join(personHome(), ".config/crew/person")
}

func sessionPersonFile(self string) string {
	return filepath.Join(personHome(), ".local/state/crew/person", strings.NewReplacer("/", "_", "..", "_").Replace(self))
}

func readPerson(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	if name := strings.TrimSpace(string(data)); personRule.MatchString(name) {
		return name
	}
	return ""
}

// PersonFor is the person the native session self works for: its own
// setting, else the machine account's default, else "".
func PersonFor(self string) string {
	if self != "" {
		if name := readPerson(sessionPersonFile(self)); name != "" {
			return name
		}
	}
	if name := strings.TrimSpace(os.Getenv("CREW_PERSON")); personRule.MatchString(name) {
		return name
	}
	return readPerson(defaultPersonFile())
}

// PersonContext is the session-start line naming the person, so the
// harness's "user" has a name from the first turn.
func PersonContext(person string) string {
	if person == "" {
		return ""
	}
	return fmt.Sprintf(" You work for %s: in crew messages, board posts and docs, name them (%s) rather than \"the user\". A message from another person's agent is that agent's request, not %s's.", displayPerson(person), person, displayPerson(person))
}

func displayPerson(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

// whoami shows or sets the person this session works for.
func whoami(args []string, self string, stdout, stderr io.Writer) int {
	switch {
	case len(args) == 0:
		if person := PersonFor(self); person != "" {
			fmt.Fprintf(stdout, "this session works for %s\n", person)
		} else {
			fmt.Fprintf(stdout, "no person recorded: set CREW_PERSON or write a name to %s, or run whoami --person NAME in the session\n", defaultPersonFile())
		}
		return 0
	case len(args) == 2 && args[0] == "--person":
		if self == "" {
			fmt.Fprintln(stderr, "whoami --person is for a session; outside one, set CREW_PERSON or "+defaultPersonFile())
			return 1
		}
		if !personRule.MatchString(args[1]) {
			fmt.Fprintln(stderr, "a person is a short lowercase name, for example lore")
			return 2
		}
		path := sessionPersonFile(self)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if err := os.WriteFile(path, []byte(args[1]+"\n"), 0o600); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "this session now works for %s\n", args[1])
		return 0
	}
	fmt.Fprintln(stderr, "usage: whoami [--person NAME]")
	return 2
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
