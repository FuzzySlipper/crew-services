package humans_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crew-services/internal/humans"
	"crew-services/internal/humans/humanshttp"
	"crew-services/internal/humans/humanssqlite"
	"crew-services/internal/humansclient"
)

// fakeHA stands in for Home Assistant's REST API and records each push.
type fakeHA struct {
	mu     sync.Mutex
	pushes []map[string]any
	paths  []string
	status int
}

func (f *fakeHA) serve(t *testing.T) *httptest.Server {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer ha-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.paths = append(f.paths, r.URL.Path)
		if f.status != 0 {
			w.WriteHeader(f.status)
			return
		}
		f.pushes = append(f.pushes, body)
		_, _ = w.Write([]byte("[]"))
	}))
	t.Cleanup(server.Close)
	return server
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func setup(t *testing.T, ha *httptest.Server) (*humans.Service, *humans.Deliverer, *clock) {
	t.Helper()
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	if err := os.WriteFile(token, []byte("ha-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	people := humans.PeopleFile{Site: "den", People: []humans.Person{
		{Name: "patch", Display: "Patch", Routes: []humans.RouteConfig{{Kind: humans.RouteHomeAssistant, URL: ha.URL, Service: "mobile_app_pixel_10_pro_xl", TokenFile: token}}},
		{Name: "lore", Display: "Lore", Routes: []humans.RouteConfig{{Kind: humans.RouteInbox}}},
	}}
	store, err := humanssqlite.Open(context.Background(), filepath.Join(dir, "humans.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	c := &clock{t: time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC)}
	svc, err := humans.New(store, c.now, humans.Config{People: people})
	if err != nil {
		t.Fatal(err)
	}
	deliverer := humans.NewDeliverer(svc, store, map[string]humans.Pusher{humans.RouteHomeAssistant: humans.HomeAssistant{}}, c.now)
	return svc, deliverer, c
}

func reply(person, key string) humans.NotifyRequest {
	return humans.NotifyRequest{Person: person, Source: "system/crew-board", IdempotencyKey: key, Title: "Reply from crew-services.lead in \"Task requests\"", Text: "Filed as 10600. " + strings.Repeat("More words here. ", 20), Link: "https://board.dragonden.stream/t/den-1#den-2", Reason: "reply", Group: "den-1"}
}

func TestANoticeIsRecordedAndPushedOnce(t *testing.T) {
	ha := &fakeHA{}
	svc, deliverer, _ := setup(t, ha.serve(t))
	ctx := context.Background()
	result, replayed, err := svc.Notify(ctx, reply("patch", "post/den-2/patch"))
	if err != nil || replayed || result.Outcome != humans.OutcomeAccepted || strings.Join(result.Routes, ",") != "inbox,home-assistant" {
		t.Fatalf("notify = %+v %v %v", result, replayed, err)
	}
	// The same key again answers the first notice and queues nothing more.
	again, replayed, err := svc.Notify(ctx, reply("patch", "post/den-2/patch"))
	if err != nil || !replayed || again.NoticeID != result.NoticeID {
		t.Fatalf("replay = %+v %v %v", again, replayed, err)
	}
	changed := reply("patch", "post/den-2/patch")
	changed.Title = "something else"
	if _, _, err := svc.Notify(ctx, changed); humans.CodeOf(err) != humans.CodeConflict {
		t.Fatalf("a reused key with new content = %v", err)
	}
	if settled := deliverer.Pass(ctx); settled != 1 {
		t.Fatalf("pass settled %d", settled)
	}
	deliverer.Pass(ctx)
	if len(ha.pushes) != 1 || ha.paths[0] != "/api/services/notify/mobile_app_pixel_10_pro_xl" {
		t.Fatalf("pushes = %v %v", ha.paths, ha.pushes)
	}
	push := ha.pushes[0]
	data := push["data"].(map[string]any)
	if push["title"] != "Reply from crew-services.lead in \"Task requests\"" || len([]rune(push["message"].(string))) > 140 ||
		data["clickAction"] != "https://board.dragonden.stream/t/den-1#den-2" || data["tag"] != "crew-den-1" || data["channel"] != "Crew" {
		t.Fatalf("push = %v", push)
	}
	inbox, err := svc.Notices(ctx, "patch", true, "", 0)
	if err != nil || inbox.Unread != 1 || len(inbox.Notices) != 1 || inbox.Notices[0].Deliveries[0].State != humans.DeliverySent {
		t.Fatalf("inbox = %+v %v", inbox, err)
	}
}

func TestQuietNoticesStayInTheInbox(t *testing.T) {
	ha := &fakeHA{}
	svc, deliverer, _ := setup(t, ha.serve(t))
	ctx := context.Background()
	follow := reply("patch", "post/den-3/patch")
	follow.Reason = humans.ReasonSubscription
	result, _, err := svc.Notify(ctx, follow)
	if err != nil || result.Outcome != humans.OutcomeRecorded {
		t.Fatalf("a subscription notice = %+v %v", result, err)
	}
	// Urgent overrides the quiet reason, on its own channel.
	follow.IdempotencyKey, follow.Urgent = "post/den-4/patch", true
	if result, _, _ := svc.Notify(ctx, follow); result.Outcome != humans.OutcomeAccepted {
		t.Fatalf("an urgent subscription notice = %+v", result)
	}
	// A person with only an inbox gets everything recorded and nothing pushed.
	if result, _, _ := svc.Notify(ctx, reply("lore", "post/den-2/lore")); result.Outcome != humans.OutcomeRecorded {
		t.Fatalf("lore = %+v", result)
	}
	deliverer.Pass(ctx)
	if len(ha.pushes) != 1 {
		t.Fatalf("pushes = %v", ha.pushes)
	}
	data := ha.pushes[0]["data"].(map[string]any)
	if data["channel"] != "Crew urgent" || data["importance"] != "high" || !strings.HasPrefix(ha.pushes[0]["title"].(string), "[urgent] ") {
		t.Fatalf("urgent push = %v", ha.pushes[0])
	}
	if _, _, err := svc.Notify(ctx, reply("nobody", "x")); humans.CodeOf(err) != humans.CodeUnknownPerson {
		t.Fatalf("unknown person = %v", err)
	}
}

func TestAFailingRouteIsRetriedThenGivenUp(t *testing.T) {
	ha := &fakeHA{status: http.StatusBadGateway}
	svc, deliverer, c := setup(t, ha.serve(t))
	ctx := context.Background()
	result, _, _ := svc.Notify(ctx, reply("patch", "k1"))
	deliverer.Pass(ctx)
	notice, _ := svc.GetNotice(ctx, result.NoticeID)
	if d := notice.Deliveries[0]; d.State != humans.DeliveryPending || d.Attempts != 1 || !strings.Contains(d.LastError, "502") {
		t.Fatalf("after a 502: %+v", d)
	}
	// Not due again until its backoff passes.
	deliverer.Pass(ctx)
	if len(ha.paths) != 1 {
		t.Fatalf("retried before its backoff: %v", ha.paths)
	}
	c.t = c.t.Add(2 * time.Hour)
	deliverer.Pass(ctx)
	notice, _ = svc.GetNotice(ctx, result.NoticeID)
	if d := notice.Deliveries[0]; d.State != humans.DeliveryFailed || !strings.HasPrefix(d.LastError, "gave up") {
		t.Fatalf("after giving up: %+v", d)
	}
	// A refusal fails at once, and the notice is still in the inbox.
	ha.status = http.StatusBadRequest
	result, _, _ = svc.Notify(ctx, reply("patch", "k2"))
	deliverer.Pass(ctx)
	notice, _ = svc.GetNotice(ctx, result.NoticeID)
	if d := notice.Deliveries[0]; d.State != humans.DeliveryFailed || !strings.Contains(d.LastError, "refused") {
		t.Fatalf("after a 400: %+v", d)
	}
	if inbox, _ := svc.Notices(ctx, "patch", false, "", 0); len(inbox.Notices) != 2 {
		t.Fatalf("inbox = %+v", inbox)
	}
}

func TestTheClientAndHandler(t *testing.T) {
	ha := &fakeHA{}
	svc, _, _ := setup(t, ha.serve(t))
	api := httptest.NewServer(humanshttp.NewHandler(svc))
	defer api.Close()
	client, err := humansclient.New(api.URL)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	site, people, err := client.People(ctx)
	if err != nil || site != "den" || len(people) != 2 || people[0].Display != "Patch" {
		t.Fatalf("people = %s %+v %v", site, people, err)
	}
	// Reads never show a token file.
	response, _ := http.Get(api.URL + "/v1/people/patch")
	var raw map[string]any
	_ = json.NewDecoder(response.Body).Decode(&raw)
	response.Body.Close()
	if text, _ := json.Marshal(raw); strings.Contains(string(text), "token") {
		t.Fatalf("a read shows the token file: %s", text)
	}
	first, err := client.Notify(ctx, humansclient.NotifyRequest(reply("lore", "a")))
	if err != nil || first.Outcome != "recorded" {
		t.Fatalf("notify = %+v %v", first, err)
	}
	second, _ := client.Notify(ctx, humansclient.NotifyRequest(reply("lore", "b")))
	var refused *humansclient.HTTPError
	if _, err := client.Notify(ctx, humansclient.NotifyRequest(reply("kate", "c"))); !errors.As(err, &refused) || refused.Code != "unknown_person" {
		t.Fatalf("unknown person over HTTP = %v", err)
	}
	inbox, err := client.Notices(ctx, "lore", true, "", 10)
	if err != nil || inbox.Unread != 2 || inbox.Notices[0].ID != second.NoticeID {
		t.Fatalf("inbox = %+v %v", inbox, err)
	}
	if err := client.MarkRead(ctx, "lore", nil, first.NoticeID, ""); err != nil {
		t.Fatal(err)
	}
	if inbox, _ := client.Notices(ctx, "lore", true, "", 10); inbox.Unread != 1 || inbox.Notices[0].ID != second.NoticeID {
		t.Fatalf("after marking through the first: %+v", inbox)
	}
	if err := client.MarkRead(ctx, "lore", []string{second.NoticeID}, "", ""); err != nil {
		t.Fatal(err)
	}
	third, _ := client.Notify(ctx, humansclient.NotifyRequest(reply("lore", "d")))
	other := reply("lore", "e")
	other.Group = "den-9"
	fourth, _ := client.Notify(ctx, humansclient.NotifyRequest(other))
	if err := client.MarkRead(ctx, "lore", nil, "", "den-1"); err != nil {
		t.Fatal(err)
	}
	if inbox, _ := client.Notices(ctx, "lore", true, "", 10); inbox.Unread != 1 || inbox.Notices[0].ID != fourth.NoticeID || third.NoticeID == "" {
		t.Fatalf("after marking group den-1: %+v", inbox)
	}
	if err := client.MarkRead(ctx, "lore", nil, "", "den-9"); err != nil {
		t.Fatal(err)
	}
	if inbox, _ := client.Notices(ctx, "lore", false, "", 10); inbox.Unread != 0 || len(inbox.Notices) != 4 {
		t.Fatalf("after marking all: %+v", inbox)
	}
}

func TestThePeopleFileIsChecked(t *testing.T) {
	for name, file := range map[string]humans.PeopleFile{
		"no site":       {People: []humans.Person{{Name: "patch"}}},
		"bad name":      {Site: "den", People: []humans.Person{{Name: "Patch"}}},
		"twice":         {Site: "den", People: []humans.Person{{Name: "patch"}, {Name: "patch"}}},
		"unknown route": {Site: "den", People: []humans.Person{{Name: "patch", Routes: []humans.RouteConfig{{Kind: "pager"}}}}},
		"no token":      {Site: "den", People: []humans.Person{{Name: "patch", Routes: []humans.RouteConfig{{Kind: humans.RouteHomeAssistant, URL: "http://ha:8123", Service: "mobile_app_x"}}}}},
		"bad push":      {Site: "den", People: []humans.Person{{Name: "patch", Routes: []humans.RouteConfig{{Kind: humans.RouteHomeAssistant, URL: "http://ha:8123", Service: "mobile_app_x", TokenFile: "t", Push: "loud"}}}}},
	} {
		if err := file.Check(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	file := humans.PeopleFile{Site: "den", People: []humans.Person{{Name: "lore"}}}
	if err := file.Check(); err != nil || file.People[0].Display != "lore" || file.People[0].Routes[0].Kind != humans.RouteInbox {
		t.Fatalf("defaults = %+v %v", file, err)
	}
}
