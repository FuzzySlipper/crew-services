package serve

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"crew-services/internal/devserver"
)

type fakeSessionLister struct {
	sessions []devserver.SessionState
	err      error
	session  devserver.SessionState
	calls    []string
}

func (f *fakeSessionLister) List(context.Context) ([]devserver.SessionState, error) {
	return f.sessions, f.err
}

func (f *fakeSessionLister) Status(context.Context, devserver.StatusOptions) (devserver.SessionState, error) {
	f.calls = append(f.calls, "status")
	return f.session, nil
}

func (f *fakeSessionLister) Stop(context.Context, devserver.StopOptions) (devserver.StopResult, error) {
	f.calls = append(f.calls, "stop")
	return devserver.StopResult{}, nil
}

func (f *fakeSessionLister) Up(context.Context, devserver.UpOptions) (devserver.UpResult, error) {
	f.calls = append(f.calls, "up")
	return devserver.UpResult{}, nil
}

func TestStatusPageListsOnlyRunningProjectsWithClickableAssignments(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{sessions: []devserver.SessionState{
		{Project: "zeta", RepoRoot: "/repos/zeta", Status: "running", Ownership: "broker_owned", Port: 37302, LANURL: "http://192.168.1.22:37302/", Health: devserver.HealthResult{Matched: true}},
		{Project: "unreachable", Status: "stopped", Ownership: "broker_owned", Port: 37301, LANURL: "http://192.168.1.22:37301/"},
		{Project: "external", Status: "running", Ownership: "unowned", Port: 37303, LANURL: "http://192.168.1.22:37303/", Health: devserver.HealthResult{Matched: true}},
		{Project: "alpha", Status: "running", Ownership: "broker_owned", Port: 5173, LANURL: "http://192.168.1.22:5173/", Health: devserver.HealthResult{Matched: true}},
	}})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	page.clock = func() time.Time { return time.Date(2026, time.August, 12, 10, 0, 0, 0, time.UTC) }

	request := httptest.NewRequest(http.MethodGet, "http://192.168.1.22:37299/", nil)
	response := httptest.NewRecorder()
	page.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", response.Code)
	}
	body := response.Body.String()
	for _, marker := range []string{
		`href="http://192.168.1.22:5173/">alpha</a>`,
		`href="http://192.168.1.22:37303/">external</a>`,
		`href="http://192.168.1.22:37302/">zeta</a>`,
		"Refreshed 2026-08-12T10:00:00Z",
		`name="repo_root" value="/repos/zeta"`,
		">Restart</button>",
		">External</td>",
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("body missing %q:\n%s", marker, body)
		}
	}
	for _, excluded := range []string{"unreachable"} {
		if strings.Contains(body, ">"+excluded+"</a>") {
			t.Fatalf("body includes excluded %s session:\n%s", excluded, body)
		}
	}
	if strings.Index(body, ">alpha</a>") > strings.Index(body, ">zeta</a>") {
		t.Fatalf("projects are not sorted:\n%s", body)
	}
	if got := response.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", got)
	}
}

func TestStatusPageEscapesProjectNamesAndUsesRequestHostFallback(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{sessions: []devserver.SessionState{
		{Project: `<script>alert("no")</script>`, Status: "running", Ownership: "broker_owned", Port: 4040, Health: devserver.HealthResult{Matched: true}},
	}})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://den-box.local:37299/", nil))
	body := response.Body.String()
	if strings.Contains(body, "<script>") {
		t.Fatalf("project name was not escaped:\n%s", body)
	}
	if !strings.Contains(body, `href="http://den-box.local:4040/"`) {
		t.Fatalf("body missing request-host fallback URL:\n%s", body)
	}
}

func TestStatusPageUsesRefreshedManagerIdentityHealthRegardlessOfStaleProcessMetadata(t *testing.T) {
	healthServer := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		response.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(healthServer.Close)
	healthURL, err := url.Parse(healthServer.URL)
	if err != nil {
		t.Fatalf("url.Parse() error = %v", err)
	}
	port, err := strconv.Atoi(healthURL.Port())
	if err != nil {
		t.Fatalf("strconv.Atoi() error = %v", err)
	}

	root := t.TempDir()
	cfg := devserver.ManagerConfig{
		StateDir:    filepath.Join(root, "state"),
		SessionRoot: filepath.Join(root, "sessions"),
		BindHost:    devserver.DefaultBindHost,
		ProbeHost:   healthURL.Hostname(),
		PublicHost:  "192.168.1.22",
		PortRange:   devserver.PortRange{Start: 37300, End: 37450},
		Timeouts: devserver.TimeoutConfig{
			LockTimeout:     time.Second,
			StartupTimeout:  time.Second,
			HealthTimeout:   time.Second,
			HealthInterval:  time.Millisecond,
			ShutdownTimeout: time.Second,
		},
	}
	manager, err := devserver.NewManager(cfg)
	if err != nil {
		t.Fatalf("NewManager() error = %v", err)
	}
	store := devserver.NewSessionStore(cfg.SessionRoot)
	writeSession := func(project string, ownership string, pid int, launchFingerprint devserver.LaunchFingerprint) {
		t.Helper()
		sessionKey := devserver.SessionKey(project, root)
		session := devserver.SessionState{
			Project:           project,
			RepoRoot:          root,
			ProbeHost:         healthURL.Hostname(),
			PublicHost:        "192.168.1.22",
			Port:              port,
			LocalURL:          healthServer.URL + "/",
			LANURL:            "http://192.168.1.22:" + strconv.Itoa(port) + "/",
			HealthURL:         healthServer.URL + "/",
			PID:               pid,
			Ownership:         ownership,
			LaunchFingerprint: launchFingerprint,
			StatePath:         store.CurrentPath(sessionKey),
		}
		if err := store.WriteCurrent(session); err != nil {
			t.Fatalf("WriteCurrent(%s) error = %v", project, err)
		}
	}
	manifest := &devserver.ServeManifest{
		Project:    "owned",
		ProbeHost:  healthURL.Hostname(),
		HealthPath: "/",
	}
	fingerprint, err := devserver.ResolveLaunchFingerprint(context.Background(), manifest)
	if err != nil {
		t.Fatalf("ResolveLaunchFingerprint() error = %v", err)
	}
	writeSession("owned", "broker_owned", syscall.Getpgrp(), fingerprint)
	manifest.Project = "external"
	externalFingerprint, err := devserver.ResolveLaunchFingerprint(context.Background(), manifest)
	if err != nil {
		t.Fatalf("ResolveLaunchFingerprint() error = %v", err)
	}
	writeSession("external", "unowned", 0, externalFingerprint)
	writeSession("stale", "broker_owned", syscall.Getpgrp(), devserver.LaunchFingerprint{Value: "old"})
	writeSession("stopped", "broker_owned", 99999999, fingerprint)

	page, err := NewStatusPage(manager)
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://192.168.1.22:37299/", nil))
	body := response.Body.String()
	for _, included := range []string{"owned", "external", "stale", "stopped"} {
		if !strings.Contains(body, ">"+included+"</a>") {
			t.Fatalf("body missing identity-healthy %s session:\n%s", included, body)
		}
	}
}

func TestStatusPageRewritesWildcardAndLoopbackLANURLsUsingRequestHost(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{sessions: []devserver.SessionState{
		{
			Project:    "rusty-dagger",
			Status:     "stopped",
			PublicHost: "192.168.1.99",
			Port:       4174,
			LANURL:     "http://0.0.0.0:4174/",
			Health:     devserver.HealthResult{Matched: true},
		},
		{
			Project:    "loopback-demo",
			Status:     "running",
			PublicHost: "192.168.1.99",
			Port:       4175,
			LANURL:     "http://127.0.0.1:4175/",
			Health:     devserver.HealthResult{Matched: true},
		},
	}})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://192.168.1.22:37299/", nil))
	body := response.Body.String()
	for _, marker := range []string{
		`href="http://192.168.1.22:4174/">rusty-dagger</a>`,
		`href="http://192.168.1.22:4175/">loopback-demo</a>`,
	} {
		if !strings.Contains(body, marker) {
			t.Fatalf("body missing rewritten link %q:\n%s", marker, body)
		}
	}
}

func TestStatusPageReturnsErrorWhenLiveListFails(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{err: errors.New("state unavailable")})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "http://localhost/", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", response.Code)
	}
}

func TestStatusPageRejectsMutatingMethods(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "http://localhost/", nil))
	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", response.Code)
	}
}

func TestStatusPageRestartsBrokerOwnedProjectAndRedirects(t *testing.T) {
	manager := &fakeSessionLister{session: devserver.SessionState{
		Project:   "alpha",
		RepoRoot:  "/repos/alpha",
		Status:    "running",
		Ownership: "broker_owned",
	}}
	manager.sessions = []devserver.SessionState{manager.session}
	manager.sessions[0].Health.Matched = true
	page, err := NewStatusPage(manager)
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader("project=alpha&repo_root=%2Frepos%2Falpha"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	page.ServeHTTP(response, request)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", response.Code, response.Body.String())
	}
	if got := response.Header().Get("Location"); got != "/?restarted=alpha" {
		t.Fatalf("Location = %q, want restart notice", got)
	}
	if got := strings.Join(manager.calls, ","); got != "status,stop,up" {
		t.Fatalf("manager calls = %q, want status,stop,up", got)
	}
}

func TestStatusPageRestartRequiresProjectAndRepoRoot(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/restart", nil))
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", response.Code)
	}
}

func TestStatusPageRestartRejectsProjectThatIsNotOnCurrentListing(t *testing.T) {
	manager := &fakeSessionLister{session: devserver.SessionState{
		Project:   "hidden",
		RepoRoot:  "/repos/hidden",
		Status:    "running",
		Ownership: "broker_owned",
	}}
	page, err := NewStatusPage(manager)
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	request := httptest.NewRequest(http.MethodPost, "/restart", strings.NewReader("project=hidden&repo_root=%2Frepos%2Fhidden"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()

	page.ServeHTTP(response, request)

	if response.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409", response.Code)
	}
	if len(manager.calls) != 0 {
		t.Fatalf("restart manager calls = %v, want none", manager.calls)
	}
}

func TestStatusPageShowsRestartNotice(t *testing.T) {
	page, err := NewStatusPage(&fakeSessionLister{})
	if err != nil {
		t.Fatalf("NewStatusPage() error = %v", err)
	}
	response := httptest.NewRecorder()
	page.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?restarted=alpha", nil))
	if !strings.Contains(response.Body.String(), "alpha restarted") {
		t.Fatalf("body missing restart notice: %s", response.Body.String())
	}
}
