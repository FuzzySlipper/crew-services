package serve

import (
	"context"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"crew-services/internal/devserver"
)

type SessionLister interface {
	List(context.Context) ([]devserver.SessionState, error)
}

type StatusPageManager interface {
	SessionLister
	RestartManager
}

type StatusPage struct {
	sessions SessionLister
	restart  *RestartService
	template *template.Template
	clock    func() time.Time
}

type StatusPageProject struct {
	Project string
	// Label adds an instance name for hosts owned by another tool, such as a playtest session.
	Label      string
	RepoRoot   string
	Port       int
	URL        string
	CanRestart bool
}

type StatusPageData struct {
	Projects  []StatusPageProject
	Refreshed string
	Notice    string
}

func NewStatusPage(manager StatusPageManager) (*StatusPage, error) {
	if manager == nil {
		return nil, fmt.Errorf("status page manager is required")
	}
	pageTemplate, err := template.New("status-page").Parse(statusPageHTML)
	if err != nil {
		return nil, fmt.Errorf("parsing status page template: %w", err)
	}
	return &StatusPage{
		sessions: manager,
		restart:  NewRestartService(manager),
		template: pageTemplate,
		clock:    time.Now,
	}, nil
}

func (p *StatusPage) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost && request.URL.Path == "/restart" {
		p.serveRestart(response, request)
		return
	}
	if request.Method != http.MethodGet || request.URL.Path != "/" {
		response.Header().Set("Allow", http.MethodGet+", "+http.MethodPost)
		http.Error(response, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	sessions, err := p.sessions.List(request.Context())
	if err != nil {
		http.Error(response, "could not read den-serve sessions", http.StatusInternalServerError)
		return
	}
	projects := reachableProjects(sessions, request.Host)
	data := StatusPageData{
		Projects:  projects,
		Refreshed: p.clock().UTC().Format(time.RFC3339),
	}
	if project := strings.TrimSpace(request.URL.Query().Get("restarted")); project != "" {
		data.Notice = project + " restarted"
	}
	response.Header().Set("Content-Type", "text/html; charset=utf-8")
	response.Header().Set("Cache-Control", "no-store")
	if err := p.template.Execute(response, data); err != nil {
		return
	}
}

func (p *StatusPage) serveRestart(response http.ResponseWriter, request *http.Request) {
	project := strings.TrimSpace(request.FormValue("project"))
	repoRoot := strings.TrimSpace(request.FormValue("repo_root"))
	if project == "" || repoRoot == "" {
		http.Error(response, "project and repo_root are required", http.StatusBadRequest)
		return
	}
	sessions, err := p.sessions.List(request.Context())
	if err != nil {
		http.Error(response, "could not verify den-serve session", http.StatusInternalServerError)
		return
	}
	if !isRestartableListing(sessions, project, repoRoot) {
		http.Error(response, "project is not a restartable den-serve listing", http.StatusConflict)
		return
	}
	if _, err := p.restart.Restart(request.Context(), devserver.UpOptions{
		Project:  project,
		RepoRoot: repoRoot,
	}); err != nil {
		http.Error(response, "could not restart "+project+": "+err.Error(), http.StatusConflict)
		return
	}
	http.Redirect(response, request, "/?restarted="+url.QueryEscape(project), http.StatusSeeOther)
}

func isRestartableListing(sessions []devserver.SessionState, project string, repoRoot string) bool {
	for _, session := range sessions {
		if session.Project == project && session.RepoRoot == repoRoot && session.Instance == "" && session.Health.Matched && session.Ownership == "broker_owned" {
			return true
		}
	}
	return false
}

func reachableProjects(sessions []devserver.SessionState, requestHost string) []StatusPageProject {
	projects := make([]StatusPageProject, 0, len(sessions))
	for _, session := range sessions {
		if !session.Health.Matched {
			continue
		}
		label := session.Project
		if session.Instance != "" {
			label += " · " + session.Instance
		}
		projects = append(projects, StatusPageProject{
			Project:    session.Project,
			Label:      label,
			RepoRoot:   session.RepoRoot,
			Port:       session.Port,
			URL:        statusProjectURL(session, requestHost),
			CanRestart: session.Ownership == "broker_owned" && session.Instance == "",
		})
	}
	sort.Slice(projects, func(left int, right int) bool {
		if projects[left].Project == projects[right].Project {
			return projects[left].Port < projects[right].Port
		}
		return projects[left].Project < projects[right].Project
	})
	return projects
}

func statusProjectURL(session devserver.SessionState, requestHost string) string {
	if isUsableLANURL(session.LANURL) {
		return session.LANURL
	}
	host := hostname(requestHost)
	if host == "" {
		host = strings.TrimSpace(session.PublicHost)
	}
	if host == "" || session.Port < 1 || session.Port > 65535 {
		return session.LocalURL
	}
	return (&url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(session.Port)),
		Path:   "/",
	}).String()
}

func isUsableLANURL(raw string) bool {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := parsed.Hostname()
	if strings.EqualFold(host, "localhost") {
		return false
	}
	if ip := net.ParseIP(host); ip != nil && (ip.IsUnspecified() || ip.IsLoopback()) {
		return false
	}
	return true
}

func hostname(hostPort string) string {
	hostPort = strings.TrimSpace(hostPort)
	if hostPort == "" {
		return ""
	}
	host, _, err := net.SplitHostPort(hostPort)
	if err == nil {
		return host
	}
	return strings.Trim(hostPort, "[]")
}

const statusPageHTML = `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>den-serve projects</title>
  <style>
    :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, sans-serif; }
    body { max-width: 52rem; margin: 3rem auto; padding: 0 1rem; }
    header { display: flex; align-items: baseline; justify-content: space-between; gap: 1rem; }
    table { width: 100%; border-collapse: collapse; margin-top: 1.5rem; }
    th, td { padding: .75rem; border-bottom: 1px solid color-mix(in srgb, currentColor 20%, transparent); text-align: left; }
    th:nth-child(2), td:nth-child(2) { text-align: right; }
    a { color: LinkText; }
    button { font: inherit; padding: .35rem .7rem; cursor: pointer; }
    .actions { width: 1%; white-space: nowrap; text-align: right; }
    .notice { padding: .75rem; margin-top: 1rem; background: color-mix(in srgb, CanvasText 8%, Canvas); }
    .empty { padding: 2rem .75rem; text-align: center !important; opacity: .7; }
    footer { margin-top: 1.5rem; opacity: .65; font-size: .85rem; }
  </style>
</head>
<body>
  <header><h1>Running den-serve projects</h1><a href="/">Refresh</a></header>
  {{if .Notice}}<div class="notice" role="status">{{.Notice}}</div>{{end}}
  <table>
    <thead><tr><th>Project</th><th>Port</th><th class="actions">Action</th></tr></thead>
    <tbody>
      {{range .Projects}}<tr><td><a href="{{.URL}}">{{.Label}}</a></td><td>{{.Port}}</td><td class="actions">{{if .CanRestart}}<form method="post" action="/restart"><input type="hidden" name="project" value="{{.Project}}"><input type="hidden" name="repo_root" value="{{.RepoRoot}}"><button type="submit">Restart</button></form>{{else if ne .Label .Project}}Owned{{else}}External{{end}}</td></tr>
      {{else}}<tr><td class="empty" colspan="3">No running projects</td></tr>{{end}}
    </tbody>
  </table>
  <footer>Refreshed {{.Refreshed}}</footer>
</body>
</html>`
