package humans

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"regexp"
)

// PeopleFile is the typed configuration of a site's people.
type PeopleFile struct {
	Site   string   `json:"site"`
	People []Person `json:"people"`
}

var nameRule = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// LoadPeople reads and checks the people file.
func LoadPeople(path string) (PeopleFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return PeopleFile{}, fmt.Errorf("read people file: %w", err)
	}
	var file PeopleFile
	if err := json.Unmarshal(data, &file); err != nil {
		return PeopleFile{}, fmt.Errorf("people file %s: %w", path, err)
	}
	if err := file.Check(); err != nil {
		return PeopleFile{}, fmt.Errorf("people file %s: %w", path, err)
	}
	return file, nil
}

// Check refuses a people file crew-humans could not act on, and fills in
// defaults: a display name, an inbox route, and "mentions" pushes.
func (f *PeopleFile) Check() error {
	if f.Site == "" {
		return fmt.Errorf("site is missing")
	}
	seen := map[string]bool{}
	for i := range f.People {
		p := &f.People[i]
		if !nameRule.MatchString(p.Name) {
			return fmt.Errorf("person %q: a name is lowercase letters, digits and hyphens", p.Name)
		}
		if seen[p.Name] {
			return fmt.Errorf("person %q is listed twice", p.Name)
		}
		seen[p.Name] = true
		if p.Display == "" {
			p.Display = p.Name
		}
		inbox := false
		for j := range p.Routes {
			r := &p.Routes[j]
			switch r.Kind {
			case RouteInbox:
				inbox = true
				continue
			case RouteHomeAssistant:
				if u, err := url.Parse(r.URL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					return fmt.Errorf("person %s, route %d: url must be Home Assistant's http(s) address", p.Name, j)
				}
				if !regexp.MustCompile(`^[a-z0-9_]+$`).MatchString(r.Service) {
					return fmt.Errorf("person %s, route %d: service must be a notify service name, for example mobile_app_pixel_10_pro_xl", p.Name, j)
				}
				if r.TokenFile == "" {
					return fmt.Errorf("person %s, route %d: token_file is missing", p.Name, j)
				}
			default:
				return fmt.Errorf("person %s, route %d: unknown kind %q (inbox, home-assistant)", p.Name, j, r.Kind)
			}
			switch r.Push {
			case "":
				r.Push = PushMentions
			case PushMentions, PushAll, PushUrgent:
			default:
				return fmt.Errorf("person %s, route %d: push must be mentions, all or urgent", p.Name, j)
			}
		}
		if !inbox {
			p.Routes = append([]RouteConfig{{Kind: RouteInbox}}, p.Routes...)
		}
	}
	return nil
}
