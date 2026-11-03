package board

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"crew-services/internal/fabricclient"
)

// Participant is who a mention names.
func (m Mention) Participant() Participant {
	return Participant{Kind: m.Kind, Address: m.Address, Site: m.Site}
}

// activation is the fabric activation policy for a notice reason: being
// named or answered waits for the next turn; following a thread never wakes.
func activation(reason string) string {
	if reason == ReasonSubscription {
		return "no_wake"
	}
	return "next_turn"
}

// noticesFor works out who hears about a new post: whoever it mentions, the
// author of the post it answers (replyTo, with reason), and whoever follows
// the thread or its board. Each participant hears once, for the strongest
// reason, and nobody hears about their own post.
func (s *Service) noticesFor(ctx context.Context, now time.Time, thread Thread, post Post, reason string, replyTo []Participant) ([]Change, error) {
	reasons := map[Participant]string{}
	var order []Participant
	add := func(p Participant, why string) {
		if current, ok := reasons[p]; ok {
			if strength(why) < strength(current) {
				reasons[p] = why
			}
			return
		}
		reasons[p] = why
		order = append(order, p)
	}
	for _, m := range post.Mentions {
		add(m.Participant(), ReasonMention)
	}
	for _, p := range replyTo {
		add(p, reason)
	}
	subscribers, err := s.store.Subscribers(ctx, thread.ID, thread.Board)
	if err != nil {
		return nil, err
	}
	for _, p := range subscribers {
		add(p, ReasonSubscription)
	}
	var changes []Change
	for _, p := range order {
		// Another site's participants hear from their own site.
		if s.isSelf(post.Author, p) || p.Site != s.cfg.Site {
			continue
		}
		changes = append(changes, Change{Notice: s.notice(now, thread, post, p, reasons[p])})
	}
	return changes, nil
}

// mentionNotices tells only the given mentions about a post, as an edit does.
func (s *Service) mentionNotices(ctx context.Context, now time.Time, thread Thread, post Post, mentions []Mention) ([]Change, error) {
	var changes []Change
	for _, m := range mentions {
		if !s.isSelf(post.Author, m.Participant()) && m.Site == s.cfg.Site {
			changes = append(changes, Change{Notice: s.notice(now, thread, post, m.Participant(), ReasonMention)})
		}
	}
	return changes, nil
}

func strength(reason string) int {
	switch reason {
	case ReasonMention:
		return 0
	case ReasonReply:
		return 1
	}
	return 2
}

// isSelf: the author never hears about their own post, and when they wrote
// as a seat, neither does the session that holds it.
func (s *Service) isSelf(author Author, p Participant) bool {
	if author.Participant() == p {
		return true
	}
	return author.Session != "" && p == Participant{Kind: AuthorSession, Address: author.Session, Site: author.Site}
}

func (s *Service) notice(now time.Time, thread Thread, post Post, p Participant, reason string) *Notice {
	n := &Notice{ID: NewID(s.cfg.Site, now), PostID: post.ID, ThreadID: thread.ID, Recipient: p, Reason: reason, Policy: activation(reason), Urgent: post.Urgent, State: NoticePending, NextAttemptAt: now, CreatedAt: now, GiveUpAt: now.Add(s.cfg.NoticeRetry)}
	if p.Kind == AuthorPerson && !s.cfg.PeopleRouted {
		// Without crew-humans, notices to people are recorded and nothing
		// more happens. Read state still sees them.
		n.State, n.LastError = NoticeNoRoute, "no route to people (crew-humans is not configured)"
	}
	return n
}

// SubscribeRequest follows, or stops following, a thread or a whole board.
type SubscribeRequest struct {
	Actor
	Thread string `json:"thread,omitempty"`
	Board  string `json:"board,omitempty"`
}

func (s *Service) subscription(ctx context.Context, request SubscribeRequest) (Subscription, error) {
	if (request.Thread == "") == (request.Board == "") {
		return Subscription{}, refuse(CodeInvalid, "give a thread or a board")
	}
	if request.Thread != "" {
		if _, err := s.thread(ctx, request.Thread); err != nil {
			return Subscription{}, err
		}
	} else if _, err := s.GetBoard(ctx, request.Board); err != nil {
		return Subscription{}, err
	}
	who, err := s.author(ctx, request.Actor)
	if err != nil {
		return Subscription{}, err
	}
	return Subscription{Participant: who.Participant(), Thread: request.Thread, Board: request.Board, CreatedAt: s.now().UTC()}, nil
}

func (s *Service) Subscribe(ctx context.Context, request SubscribeRequest) (Subscription, error) {
	sub, err := s.subscription(ctx, request)
	if err != nil {
		return Subscription{}, err
	}
	return sub, s.store.Subscribe(ctx, sub)
}

func (s *Service) Unsubscribe(ctx context.Context, request SubscribeRequest) (Subscription, error) {
	sub, err := s.subscription(ctx, request)
	if err != nil {
		return Subscription{}, err
	}
	return sub, s.store.Unsubscribe(ctx, sub)
}

// Subscriptions lists what the actor follows.
func (s *Service) Subscriptions(ctx context.Context, actor Actor) ([]Subscription, error) {
	who, err := s.author(ctx, actor)
	if err != nil {
		return nil, err
	}
	return s.store.Subscriptions(ctx, who.Participant())
}

// MarkReadRequest moves the actor's read cursor in a thread up to a post.
type MarkReadRequest struct {
	Actor
	Thread string `json:"thread"`
	Post   string `json:"post"`
}

// MarkRead records how far the actor has read. It changes nothing else: no
// notice, wake or delivery.
func (s *Service) MarkRead(ctx context.Context, request MarkReadRequest) error {
	who, err := s.author(ctx, request.Actor)
	if err != nil {
		return err
	}
	post, err := s.store.GetPost(ctx, request.Post)
	if errors.Is(err, ErrNotFound) || err == nil && post.ThreadID != request.Thread {
		return refuse(CodePostNotFound, "no post %s in thread %s", request.Post, request.Thread)
	}
	if err != nil {
		return err
	}
	return s.store.MarkRead(ctx, who.Participant(), request.Thread, request.Post, s.now().UTC())
}

// Cursor is the newest post the actor has read in a thread, or "".
func (s *Service) Cursor(ctx context.Context, actor Actor, thread string) (string, error) {
	who, err := s.author(ctx, actor)
	if err != nil {
		return "", err
	}
	return s.store.Cursor(ctx, who.Participant(), thread)
}

// Unread lists the threads the actor was told about and has not read past.
func (s *Service) Unread(ctx context.Context, actor Actor, limit int) ([]Unread, error) {
	who, err := s.author(ctx, actor)
	if err != nil {
		return nil, err
	}
	return s.store.Unread(ctx, who.Participant(), pageSize(limit))
}

// Notices lists recent notices, in one state or all.
func (s *Service) Notices(ctx context.Context, state string, limit int) ([]Notice, error) {
	switch state {
	case "", NoticePending, NoticeSent, NoticeDelivered, NoticeExpired, NoticeFailed, NoticeNoRoute:
	default:
		return nil, refuse(CodeInvalid, "state must be pending, sent, delivered, expired, failed or no_route")
	}
	return s.store.ListNotices(ctx, state, pageSize(limit))
}

// NoticeFabric is the part of the fabric a Notifier uses.
type NoticeFabric interface {
	Register(ctx context.Context, adapterID, instanceID string, duration time.Duration) (fabricclient.Lease, error)
	Renew(ctx context.Context, adapterID, token string, duration time.Duration) (fabricclient.Lease, error)
	Resolve(ctx context.Context, address string) (fabricclient.Binding, error)
	Bind(ctx context.Context, address string, request fabricclient.BindRequest) (fabricclient.Binding, error)
	Submit(ctx context.Context, request fabricclient.MessageRequest) (fabricclient.SubmittedMessage, error)
	Mailbox(ctx context.Context, address string) ([]fabricclient.Delivery, error)
}

// NotifierConfig names crew-board's producer identity on the fabric.
type NotifierConfig struct {
	// AdapterID is crew-board's fabric producer; Origin is the service-owned
	// address notices come from (nothing drives it, and replies to it are
	// never delivered).
	AdapterID, InstanceID, Origin string
	// Site is this site; a post from any other is framed as coming from
	// outside this box.
	Site          string
	LeaseDuration time.Duration
	// TTL is how long a sent notice waits in the fabric, for example for a
	// vacant seat's next holder. It must not exceed the fabric's own cap.
	TTL time.Duration
	// People takes notices to people (crew-humans); WebURL is the people's
	// pages, which a person's notice links to.
	People PeopleNotifier
	WebURL string
}

// PeopleNotifier hands a notice for a person to crew-humans, and returns the
// notice's ID there. An error wrapping ErrPersonRefused is a definite answer;
// any other is retried.
type PeopleNotifier interface {
	NotifyPerson(ctx context.Context, n PersonNotice) (string, error)
}

// ErrPersonRefused is crew-humans refusing a notice (an unknown person, a
// malformed request): retrying cannot help.
var ErrPersonRefused = errors.New("crew-humans refused the notice")

// PersonNotice is what crew-board asks crew-humans to tell a person. It is
// fixed before the first attempt, so a retry asks for exactly the same.
type PersonNotice struct {
	Person string `json:"person"`
	Key    string `json:"key"`
	Title  string `json:"title"`
	Text   string `json:"text"`
	Link   string `json:"link,omitempty"`
	Reason string `json:"reason"`
	Group  string `json:"group"`
	Urgent bool   `json:"urgent,omitempty"`
}

// Notifier sends queued notices to the fabric. Whatever happens to a notice
// is recorded on it; a post never depends on its notices.
type Notifier struct {
	store Store
	dir   Directory
	fab   NoticeFabric
	now   func() time.Time
	cfg   NotifierConfig
	wake  chan struct{}

	mu    sync.Mutex
	lease fabricclient.Lease
	bound bool
}

func NewNotifier(store Store, dir Directory, fab NoticeFabric, now func() time.Time, cfg NotifierConfig) *Notifier {
	if cfg.AdapterID == "" {
		cfg.AdapterID = "crew-board"
	}
	if cfg.InstanceID == "" {
		cfg.InstanceID = cfg.AdapterID
	}
	if cfg.Origin == "" {
		cfg.Origin = "system/crew-board"
	}
	if cfg.LeaseDuration <= 0 {
		cfg.LeaseDuration = 5 * time.Minute
	}
	if cfg.TTL <= 0 {
		cfg.TTL = 24 * time.Hour
	}
	return &Notifier{store: store, dir: dir, fab: fab, now: now, cfg: cfg, wake: make(chan struct{}, 1)}
}

// Poke asks for a send pass now.
func (n *Notifier) Poke() {
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Run sends due notices on every tick and whenever poked, until ctx ends.
func (n *Notifier) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n.Pass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-n.wake:
		}
	}
}

// Pass sends every notice that is due, and reports how many it settled.
func (n *Notifier) Pass(ctx context.Context) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	due, err := n.store.DueNotices(ctx, n.now().UTC(), 50)
	if err != nil {
		return 0
	}
	settled := 0
	for _, notice := range due {
		done := false
		if notice.State == NoticeSent {
			done = n.check(ctx, notice)
		} else {
			done = n.send(ctx, notice)
		}
		if done {
			settled++
		}
	}
	return settled
}

// never is a next check that does not come: a sent notice whose delivery did
// not settle within its check window stays sent.
var never = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// check reads what became of a sent notice's delivery: delivered, expired (in
// the fabric, or still queued past its TTL, so it never can be delivered), or
// given up on by the fabric. A delivery in flight or still waiting is checked
// again later.
func (n *Notifier) check(ctx context.Context, notice Notice) bool {
	now := n.now().UTC()
	deliveries, err := n.fab.Mailbox(ctx, notice.Recipient.Address)
	if err != nil {
		notice.NextAttemptAt = now.Add(time.Minute)
		_ = n.store.UpdateNotice(ctx, notice)
		return false
	}
	for _, d := range deliveries {
		if d.MessageID != notice.MessageID {
			continue
		}
		switch {
		case d.State == "delivered":
			notice.State, notice.LastError = NoticeDelivered, ""
		case d.State == "expired", (d.State == "queued" || d.State == "claimed") && !d.ExpiresAt.IsZero() && !d.ExpiresAt.After(now):
			notice.State, notice.LastError = NoticeExpired, "not delivered before its TTL ran out"
		case d.State == "failed" || d.State == "cancelled" || d.State == "outcome_unknown":
			notice.State, notice.LastError = NoticeFailed, "the fabric settled it as "+d.State+" "+d.TerminalReason
		}
	}
	if notice.State == NoticeSent {
		notice.NextAttemptAt = now.Add(time.Minute)
		if !now.Before(notice.GiveUpAt) {
			notice.NextAttemptAt = never
		}
	}
	_ = n.store.UpdateNotice(ctx, notice)
	return notice.State != NoticeSent
}

// send makes one attempt and records it. A refusal fails the notice at once;
// an unreachable fabric is retried with backoff until the notice gives up.
func (n *Notifier) send(ctx context.Context, notice Notice) bool {
	now := n.now().UTC()
	notice.Attempts++
	if err := n.stillShown(ctx, notice); err != nil {
		notice.State, notice.LastError = NoticeFailed, err.Error()
		_ = n.store.UpdateNotice(ctx, notice)
		return true
	}
	if notice.Recipient.Kind == AuthorPerson {
		return n.sendPerson(ctx, notice, now)
	}
	if notice.Body == "" {
		// The text is fixed before the first attempt, so a retry after a lost
		// response submits the same request and the fabric replays it,
		// whatever has happened to the post or the seat since.
		body, err := n.body(ctx, notice)
		if err != nil {
			notice.State, notice.LastError = NoticeFailed, err.Error()
			_ = n.store.UpdateNotice(ctx, notice)
			return true
		}
		notice.Body = body
		if err := n.store.UpdateNotice(ctx, notice); err != nil {
			return false
		}
	}
	messageID, err := n.submit(ctx, notice, notice.Body)
	switch {
	case err == nil:
		notice.State, notice.MessageID, notice.SentAt, notice.LastError = NoticeSent, messageID, now, ""
		// From now on the notice is checked until its delivery settles.
		notice.NextAttemptAt, notice.GiveUpAt = now.Add(time.Minute), now.Add(n.cfg.TTL+time.Hour)
	case refused(err):
		notice.State, notice.LastError = NoticeFailed, "the fabric refused it: "+errorText(err)
	case !now.Before(notice.GiveUpAt):
		notice.State, notice.LastError = NoticeFailed, "the fabric could not be reached before the notice gave up: "+errorText(err)
	default:
		notice.LastError = errorText(err)
		notice.NextAttemptAt = now.Add(backoff(notice.Attempts))
	}
	_ = n.store.UpdateNotice(ctx, notice)
	return notice.State != NoticePending
}

// sendPerson hands a person's notice to crew-humans. Once crew-humans has
// recorded it the notice is delivered: what happens on the person's routes
// is crew-humans' to record.
func (n *Notifier) sendPerson(ctx context.Context, notice Notice, now time.Time) bool {
	if n.cfg.People == nil {
		notice.State, notice.LastError = NoticeNoRoute, "no route to people (crew-humans is not configured)"
		_ = n.store.UpdateNotice(ctx, notice)
		return true
	}
	var request PersonNotice
	if notice.Body == "" {
		var err error
		if request, err = n.personNotice(ctx, notice); err != nil {
			notice.State, notice.LastError = NoticeFailed, err.Error()
			_ = n.store.UpdateNotice(ctx, notice)
			return true
		}
		body, _ := json.Marshal(request)
		notice.Body = string(body)
		if err := n.store.UpdateNotice(ctx, notice); err != nil {
			return false
		}
	} else if err := json.Unmarshal([]byte(notice.Body), &request); err != nil {
		notice.State, notice.LastError = NoticeFailed, "the stored notice is unreadable: "+err.Error()
		_ = n.store.UpdateNotice(ctx, notice)
		return true
	}
	id, err := n.cfg.People.NotifyPerson(ctx, request)
	switch {
	case err == nil:
		notice.State, notice.MessageID, notice.SentAt, notice.LastError = NoticeDelivered, id, now, ""
	case errors.Is(err, ErrPersonRefused):
		notice.State, notice.LastError = NoticeFailed, err.Error()
	case !now.Before(notice.GiveUpAt):
		notice.State, notice.LastError = NoticeFailed, "crew-humans could not be reached before the notice gave up: "+err.Error()
	default:
		notice.LastError = err.Error()
		notice.NextAttemptAt = now.Add(backoff(notice.Attempts))
	}
	_ = n.store.UpdateNotice(ctx, notice)
	return notice.State != NoticePending
}

// personNotice is a person's notice: who wrote what where, a short quote,
// and a link to the post on the people's pages.
func (n *Notifier) personNotice(ctx context.Context, notice Notice) (PersonNotice, error) {
	post, err := n.store.GetPost(ctx, notice.PostID)
	if err != nil {
		return PersonNotice{}, err
	}
	thread, err := n.store.GetThread(ctx, notice.ThreadID)
	if err != nil {
		return PersonNotice{}, err
	}
	who := personAuthor(post.Author)
	var title string
	switch notice.Reason {
	case ReasonMention:
		title = fmt.Sprintf("%s mentioned you in %q", who, thread.Title)
	case ReasonReply:
		title = fmt.Sprintf("%s replied in %q", who, thread.Title)
	default:
		title = fmt.Sprintf("New in %q, from %s", thread.Title, who)
	}
	if len([]rune(title)) > 190 {
		title = string([]rune(title)[:189]) + "…"
	}
	// The excerpt is bounded here, before the request is frozen: a post may
	// be a megabyte, and crew-humans keeps 600 characters of it anyway.
	text := strings.Join(strings.Fields(RenderForAgent(post.Body, false)), " ")
	if n.cfg.Site != "" && post.Site != n.cfg.Site {
		text = "(from site " + post.Site + ") " + text
	}
	if runes := []rune(text); len(runes) > personExcerpt {
		text = string(runes[:personExcerpt-1]) + "…"
	}
	link := ""
	if n.cfg.WebURL != "" {
		link = strings.TrimRight(n.cfg.WebURL, "/") + "/t/" + url.PathEscape(thread.ID) + "?post=" + url.QueryEscape(post.ID) + "#" + post.ID
	}
	return PersonNotice{Person: notice.Recipient.Address, Key: "crew-board/notice/" + notice.ID, Title: title, Text: text, Link: link, Reason: notice.Reason, Group: thread.ID, Urgent: notice.Urgent}, nil
}

// personExcerpt is how much of a post a person's notice quotes, in
// characters: crew-humans' own limit.
const personExcerpt = 600

// personAuthor is a post's author as a person reads it: a seat or session
// with its runtime, a person by name.
func personAuthor(a Author) string {
	if a.Kind == AuthorPerson {
		return displayPerson(a.Address)
	}
	if a.Kind == AuthorSession && a.Label != "" {
		return a.Label
	}
	return a.Address
}

func displayPerson(name string) string {
	if name == "" {
		return name
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func backoff(attempts int) time.Duration {
	d := 5 * time.Second
	for i := 1; i < attempts && d < 5*time.Minute; i++ {
		d *= 2
	}
	return min(d, 5*time.Minute)
}

// refused reports a definite answer from the fabric: a 4xx other than the
// lease and origin problems a fresh registration fixes.
func refused(err error) bool {
	var typed *fabricclient.HTTPError
	if !errors.As(err, &typed) || typed.Status < 400 || typed.Status >= 500 {
		return false
	}
	switch fabricclient.Code(err) {
	case "lease_expired", "lease_fenced", "adapter_mismatch":
		return false
	}
	return true
}

func errorText(err error) string {
	if code := fabricclient.Code(err); code != "" {
		return code
	}
	return err.Error()
}

func (n *Notifier) submit(ctx context.Context, notice Notice, body string) (string, error) {
	if err := n.ensureLease(ctx); err != nil {
		return "", err
	}
	if err := n.ensureOrigin(ctx); err != nil {
		return "", err
	}
	result, err := n.fab.Submit(ctx, fabricclient.MessageRequest{
		ProducerID:       n.cfg.AdapterID,
		LeaseToken:       n.lease.LeaseToken,
		OperationID:      "crew-board/notice/" + notice.ID,
		SenderAddress:    n.cfg.Origin,
		RecipientAddress: notice.Recipient.Address,
		Body:             body,
		ActivationPolicy: notice.Policy,
		TTL:              n.cfg.TTL.String(),
	})
	switch fabricclient.Code(err) {
	case "lease_expired", "lease_fenced":
		n.lease, n.bound = fabricclient.Lease{}, false
	case "adapter_mismatch":
		n.bound = false
	}
	if err != nil {
		return "", err
	}
	return result.Message.MessageID, nil
}

func (n *Notifier) ensureLease(ctx context.Context) error {
	now := n.now()
	if n.lease.LeaseToken != "" && now.Add(n.cfg.LeaseDuration/2).Before(n.lease.ExpiresAt) {
		return nil
	}
	if n.lease.LeaseToken != "" {
		if lease, err := n.fab.Renew(ctx, n.cfg.AdapterID, n.lease.LeaseToken, n.cfg.LeaseDuration); err == nil {
			n.lease = lease
			return nil
		}
	}
	lease, err := n.fab.Register(ctx, n.cfg.AdapterID, n.cfg.InstanceID, n.cfg.LeaseDuration)
	if err != nil {
		n.lease = fabricclient.Lease{}
		return fmt.Errorf("register with the fabric: %w", err)
	}
	n.lease, n.bound = lease, false
	return nil
}

// ensureOrigin binds system/crew-board to crew-board itself, as crew-review
// binds its own origin. No driver claims deliveries for it.
func (n *Notifier) ensureOrigin(ctx context.Context) error {
	if n.bound {
		return nil
	}
	request := fabricclient.BindRequest{ActorAdapterID: n.cfg.AdapterID, LeaseToken: n.lease.LeaseToken, AdapterID: n.cfg.AdapterID, TargetRef: n.cfg.AdapterID, Capabilities: []string{}}
	binding, err := n.fab.Resolve(ctx, n.cfg.Origin)
	switch {
	case err == nil && binding.Bound && binding.AdapterID == n.cfg.AdapterID:
		n.bound = true
		return nil
	case err == nil && binding.Bound:
		return fmt.Errorf("%s is bound to adapter %s, not %s", n.cfg.Origin, binding.AdapterID, n.cfg.AdapterID)
	case err == nil:
		revision := binding.Revision
		request.ExpectedRevision = &revision
	case !notFound(err):
		return fmt.Errorf("resolve %s: %w", n.cfg.Origin, err)
	}
	if _, err := n.fab.Bind(ctx, n.cfg.Origin, request); err != nil {
		return fmt.Errorf("bind %s: %w", n.cfg.Origin, err)
	}
	n.bound = true
	return nil
}

// stillShown refuses to send a notice for a post that was withdrawn after it
// was written.
func (n *Notifier) stillShown(ctx context.Context, notice Notice) error {
	post, err := n.store.GetPost(ctx, notice.PostID)
	if errors.Is(err, ErrNotFound) {
		return errors.New("the post is gone")
	}
	if err != nil {
		return err
	}
	if post.State != PostVisible {
		return fmt.Errorf("the post was %s before the notice went out", post.State)
	}
	return nil
}

// body is the notice text: who wrote what where and why this participant is
// hearing about it, a short quote with folds collapsed, and how to read it.
func (n *Notifier) body(ctx context.Context, notice Notice) (string, error) {
	post, err := n.store.GetPost(ctx, notice.PostID)
	if errors.Is(err, ErrNotFound) {
		return "", errors.New("the post is gone")
	}
	if err != nil {
		return "", err
	}
	if post.State != PostVisible {
		return "", fmt.Errorf("the post was %s before the notice went out", post.State)
	}
	thread, err := n.store.GetThread(ctx, notice.ThreadID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	if notice.Urgent {
		b.WriteString("[urgent] ")
	}
	what := map[string]string{ReasonMention: "You were mentioned by", ReasonReply: "Reply to your post from", ReasonSubscription: "New post in a thread you follow, from"}[notice.Reason]
	where := "board " + thread.Board
	if post.Kind != "" {
		where += ", a " + post.Kind
	}
	if n.cfg.Site != "" && post.Site != n.cfg.Site {
		where += "; from site " + post.Site + ", outside this box"
	}
	fmt.Fprintf(&b, "%s %s in %q (%s):\n\n", what, displayAuthor(post.Author), thread.Title, where)
	quote := strings.TrimSpace(RenderForAgent(post.Body, false))
	if len(quote) > 600 {
		cut := strings.LastIndex(quote[:600], " ")
		if cut < 400 {
			cut = 600
		}
		quote = quote[:cut] + " …"
	}
	for _, line := range strings.Split(quote, "\n") {
		b.WriteString("> " + line + "\n")
	}
	// A seat's commands act as the seat, so the read moves the seat's cursor,
	// the ack counts for the seat, and the answer comes from the seat.
	command, as := n.command(ctx, notice.Recipient), ""
	if notice.Recipient.Kind == AuthorSeat {
		as = " --as " + notice.Recipient.Address
	}
	fmt.Fprintf(&b, "\nRead it with: %s board read %s%s", command, post.ID, as)
	if post.Kind == KindDecision && notice.Reason == ReasonMention {
		fmt.Fprintf(&b, "\nThis decision names you; acknowledge it with: %s board ack %s%s", command, post.ID, as)
	}
	if notice.Reason != ReasonSubscription {
		fmt.Fprintf(&b, "\nAnswer on the board with: %s board reply %s%s TEXT", command, post.ID, as)
	}
	return b.String(), nil
}

// command is the driver command the recipient runs: the one behind its
// session, or behind the session holding the seat.
func (n *Notifier) command(ctx context.Context, p Participant) string {
	session := p.Address
	if p.Kind == AuthorSeat {
		holder, _, err := n.dir.SeatHolder(ctx, p.Address)
		if err != nil || holder == "" {
			return "<your driver>"
		}
		session = holder
	}
	adapter, _, err := n.dir.Session(ctx, session)
	if err != nil {
		return "<your driver>"
	}
	switch adapter {
	case "claude-code":
		return "crew-claude"
	case "crew-codex":
		return "crew-codex"
	case "crew-dsh":
		return "crew-dsh"
	case "crew-hermes":
		return "crew-hermes"
	}
	return "<your driver>"
}

func displayAuthor(a Author) string {
	runtime := map[string]string{"claude-code": "Claude Code", "crew-codex": "Codex", "crew-dsh": "DSH", "crew-hermes": "Hermes"}[a.Adapter]
	if runtime == "" {
		runtime = a.Adapter
	}
	if a.Kind == AuthorSeat && a.Label != "" {
		runtime += " " + a.Label
	}
	if a.Model != "" {
		runtime += ", " + a.Model
	}
	if a.WorkingFor != "" {
		runtime += ", working for " + displayPerson(a.WorkingFor)
	}
	switch a.Kind {
	case AuthorSeat:
		return fmt.Sprintf("%s (%s, %s)", a.Address, runtime, a.Session)
	case AuthorSession:
		if a.Label != "" {
			return fmt.Sprintf("%s (%s, %s)", a.Label, runtime, a.Address)
		}
		return fmt.Sprintf("%s (%s)", a.Address, runtime)
	}
	return a.Address
}
