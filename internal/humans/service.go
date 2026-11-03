package humans

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"
)

// Store is crew-humans' records.
type Store interface {
	// InsertNotice records a notice and its deliveries, unless the source
	// already sent this idempotency key: then it returns that notice and
	// false.
	InsertNotice(ctx context.Context, n Notice) (Notice, bool, error)
	GetNotice(ctx context.Context, id string) (Notice, error)
	// ListNotices lists a person's notices, newest first, older than before
	// (an ID) when it is given.
	ListNotices(ctx context.Context, person string, unreadOnly bool, before string, limit int) ([]Notice, error)
	CountUnread(ctx context.Context, person string) (int, error)
	// MarkRead marks the person's unread notices read: those named, every
	// one up to and including through (an ID), or every one in group.
	MarkRead(ctx context.Context, person string, ids []string, through, group string, at time.Time) (int, error)
	DueDeliveries(ctx context.Context, now time.Time, limit int) ([]Delivery, error)
	UpdateDelivery(ctx context.Context, d Delivery) error
}

// Config bounds the service.
type Config struct {
	People PeopleFile
	// GiveUp is how long a push is retried while its route cannot be reached.
	GiveUp time.Duration
}

// Service owns the notify contract.
type Service struct {
	store  Store
	now    func() time.Time
	cfg    Config
	people map[string]Person
	poke   func()
}

func New(store Store, now func() time.Time, cfg Config) (*Service, error) {
	if err := cfg.People.Check(); err != nil {
		return nil, err
	}
	if cfg.GiveUp <= 0 {
		cfg.GiveUp = time.Hour
	}
	people := map[string]Person{}
	for _, p := range cfg.People.People {
		people[p.Name] = p
	}
	return &Service{store: store, now: now, cfg: cfg, people: people, poke: func() {}}, nil
}

// OnNotice is called after a notice with pushes is recorded.
func (s *Service) OnNotice(poke func()) { s.poke = poke }

func (s *Service) Site() string { return s.cfg.People.Site }

// People lists the people, with no secrets.
func (s *Service) People() []Person {
	out := make([]Person, 0, len(s.cfg.People.People))
	for _, p := range s.cfg.People.People {
		out = append(out, public(p))
	}
	return out
}

// GetPerson reads one person, with no secrets.
func (s *Service) GetPerson(name string) (Person, error) {
	p, ok := s.people[name]
	if !ok {
		return Person{}, refuse(CodeUnknownPerson, "no person %q on site %s", name, s.cfg.People.Site)
	}
	return public(p), nil
}

func public(p Person) Person {
	routes := make([]RouteConfig, len(p.Routes))
	for i, r := range p.Routes {
		routes[i] = r.Public()
	}
	p.Routes = routes
	return p
}

// Limits on what a notice carries.
const (
	maxTitle = 200
	maxText  = 600
	maxLink  = 2048
	maxKey   = 256
)

// Notify records a notice for a person and queues it for the routes that
// push it. It answers at once; the caller never waits for delivery.
func (s *Service) Notify(ctx context.Context, r NotifyRequest) (NotifyResult, bool, error) {
	person, ok := s.people[r.Person]
	if !ok {
		return NotifyResult{}, false, refuse(CodeUnknownPerson, "no person %q on site %s", r.Person, s.cfg.People.Site)
	}
	r.Title, r.Text = strings.TrimSpace(r.Title), strings.TrimSpace(r.Text)
	switch {
	case r.Source == "" || len(r.Source) > maxKey:
		return NotifyResult{}, false, refuse(CodeInvalid, "source is the caller, for example system/crew-board")
	case r.IdempotencyKey == "" || len(r.IdempotencyKey) > maxKey:
		return NotifyResult{}, false, refuse(CodeInvalid, "idempotency_key is required, at most %d bytes", maxKey)
	case r.Title == "":
		return NotifyResult{}, false, refuse(CodeInvalid, "title is required")
	case utf8.RuneCountInString(r.Title) > maxTitle:
		return NotifyResult{}, false, refuse(CodeInvalid, "title is longer than %d characters", maxTitle)
	case len(r.Link) > maxLink:
		return NotifyResult{}, false, refuse(CodeInvalid, "link is longer than %d bytes", maxLink)
	case len(r.Group) > maxKey || len(r.Reason) > 64:
		return NotifyResult{}, false, refuse(CodeInvalid, "group or reason is too long")
	}
	if r.Link != "" {
		if u, err := url.Parse(r.Link); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return NotifyResult{}, false, refuse(CodeInvalid, "link must be an http(s) address")
		}
	}
	r.Text = cut(r.Text, maxText)
	now := s.now().UTC()
	n := Notice{ID: newID(now), Person: r.Person, Source: r.Source, IdempotencyKey: r.IdempotencyKey, Title: r.Title, Text: r.Text, Link: r.Link, Reason: r.Reason, Group: r.Group, Urgent: r.Urgent, CreatedAt: now}
	for i, route := range person.Routes {
		if route.Kind == RouteInbox || !route.pushes(n) {
			continue
		}
		n.Deliveries = append(n.Deliveries, Delivery{NoticeID: n.ID, Route: i, Kind: route.Kind, State: DeliveryPending, NextAttemptAt: now, GiveUpAt: now.Add(s.cfg.GiveUp)})
	}
	stored, created, err := s.store.InsertNotice(ctx, n)
	if err != nil {
		return NotifyResult{}, false, err
	}
	if !created && !sameRequest(stored, n) {
		return NotifyResult{}, false, refuse(CodeConflict, "%s already sent %q with different content", r.Source, r.IdempotencyKey)
	}
	if created && len(stored.Deliveries) > 0 {
		s.poke()
	}
	return result(stored), !created, nil
}

func sameRequest(a, b Notice) bool {
	return a.Person == b.Person && a.Title == b.Title && a.Text == b.Text && a.Link == b.Link && a.Reason == b.Reason && a.Group == b.Group && a.Urgent == b.Urgent
}

func result(n Notice) NotifyResult {
	out := NotifyResult{NoticeID: n.ID, Outcome: OutcomeRecorded, Routes: []string{RouteInbox}}
	for _, d := range n.Deliveries {
		out.Outcome = OutcomeAccepted
		out.Routes = append(out.Routes, d.Kind)
	}
	return out
}

// cut shortens text to at most max characters, at a word where it can.
func cut(text string, max int) string {
	if utf8.RuneCountInString(text) <= max {
		return text
	}
	runes := []rune(text)[:max-1]
	short := string(runes)
	if i := strings.LastIndexAny(short, " \n"); i > len(short)*2/3 {
		short = short[:i]
	}
	return strings.TrimRight(short, " \n") + "…"
}

// Inbox is one page of a person's notices.
type Inbox struct {
	Notices []Notice `json:"notices"`
	Unread  int      `json:"unread"`
}

// Notices lists a person's notices, newest first. Reading changes nothing.
func (s *Service) Notices(ctx context.Context, person string, unreadOnly bool, before string, limit int) (Inbox, error) {
	if _, ok := s.people[person]; !ok {
		return Inbox{}, refuse(CodeUnknownPerson, "no person %q on site %s", person, s.cfg.People.Site)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	notices, err := s.store.ListNotices(ctx, person, unreadOnly, before, limit)
	if err != nil {
		return Inbox{}, err
	}
	unread, err := s.store.CountUnread(ctx, person)
	if err != nil {
		return Inbox{}, err
	}
	if notices == nil {
		notices = []Notice{}
	}
	return Inbox{Notices: notices, Unread: unread}, nil
}

// GetNotice reads one notice.
func (s *Service) GetNotice(ctx context.Context, id string) (Notice, error) {
	n, err := s.store.GetNotice(ctx, id)
	if errors.Is(err, ErrNotFound) {
		return Notice{}, refuse(CodeNotFound, "no notice %s", id)
	}
	return n, err
}

// MarkReadRequest marks a person's notices read: the IDs given, every
// notice up to and including Through, or every notice in Group (crew-board
// marks a thread's notices read when the person opens the thread).
type MarkReadRequest struct {
	Person  string   `json:"person"`
	IDs     []string `json:"ids,omitempty"`
	Through string   `json:"through,omitempty"`
	Group   string   `json:"group,omitempty"`
}

// MarkRead marks notices read. It never redelivers, retries or tells the
// caller that sent them.
func (s *Service) MarkRead(ctx context.Context, r MarkReadRequest) (int, error) {
	if _, ok := s.people[r.Person]; !ok {
		return 0, refuse(CodeUnknownPerson, "no person %q on site %s", r.Person, s.cfg.People.Site)
	}
	given := 0
	for _, set := range []bool{len(r.IDs) > 0, r.Through != "", r.Group != ""} {
		if set {
			given++
		}
	}
	if given != 1 || len(r.IDs) > 200 {
		return 0, refuse(CodeInvalid, "give one of ids (at most 200), through or group")
	}
	return s.store.MarkRead(ctx, r.Person, r.IDs, r.Through, r.Group, s.now().UTC())
}

// route is the configured route a delivery was queued for, if the people
// file still has it.
func (s *Service) route(person string, d Delivery) (Person, RouteConfig, bool) {
	p, ok := s.people[person]
	if !ok || d.Route < 0 || d.Route >= len(p.Routes) || p.Routes[d.Route].Kind != d.Kind {
		return Person{}, RouteConfig{}, false
	}
	return p, p.Routes[d.Route], true
}
