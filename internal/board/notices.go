package board

import (
	"context"
	"errors"
	"fmt"
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
		if s.isSelf(post.Author, p) {
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
		if !s.isSelf(post.Author, m.Participant()) {
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
	if p.Kind == AuthorPerson {
		// crew-humans (#10535) will route notices to people; until then they
		// are recorded and nothing more happens. Read state still sees them.
		n.State, n.LastError = NoticeNoRoute, "no route to people yet (crew-humans)"
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
	case "", NoticePending, NoticeSent, NoticeFailed, NoticeNoRoute:
	default:
		return nil, refuse(CodeInvalid, "state must be pending, sent, failed or no_route")
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
}

// NotifierConfig names crew-board's producer identity on the fabric.
type NotifierConfig struct {
	// AdapterID is crew-board's fabric producer; Origin is the service-owned
	// address notices come from (nothing drives it, and replies to it are
	// never delivered).
	AdapterID, InstanceID, Origin string
	LeaseDuration                 time.Duration
	// TTL is how long a sent notice waits in the fabric, for example for a
	// vacant seat's next holder. It must not exceed the fabric's own cap.
	TTL time.Duration
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
		if n.send(ctx, notice) {
			settled++
		}
	}
	return settled
}

// send makes one attempt and records it. A refusal fails the notice at once;
// an unreachable fabric is retried with backoff until the notice gives up.
func (n *Notifier) send(ctx context.Context, notice Notice) bool {
	now := n.now().UTC()
	notice.Attempts++
	body, err := n.body(ctx, notice)
	if err != nil {
		notice.State, notice.LastError = NoticeFailed, err.Error()
		_ = n.store.UpdateNotice(ctx, notice)
		return true
	}
	messageID, err := n.submit(ctx, notice, body)
	switch {
	case err == nil:
		notice.State, notice.MessageID, notice.SentAt, notice.LastError = NoticeSent, messageID, now, ""
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
	fmt.Fprintf(&b, "\nRead it with: %s board read %s", n.command(ctx, notice.Recipient), post.ID)
	if post.Kind == KindDecision && notice.Reason == ReasonMention {
		fmt.Fprintf(&b, "\nThis decision names you; acknowledge it with: %s board ack %s", n.command(ctx, notice.Recipient), post.ID)
	}
	if notice.Reason != ReasonSubscription {
		fmt.Fprintf(&b, "\nAnswer on the board with: %s board reply %s TEXT", n.command(ctx, notice.Recipient), post.ID)
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
	}
	return "<your driver>"
}

func displayAuthor(a Author) string {
	runtime := map[string]string{"claude-code": "Claude Code", "crew-codex": "Codex", "crew-dsh": "DSH"}[a.Adapter]
	if runtime == "" {
		runtime = a.Adapter
	}
	switch a.Kind {
	case AuthorSeat:
		return fmt.Sprintf("%s (%s, %s)", a.Address, runtime, a.Session)
	case AuthorSession:
		return fmt.Sprintf("%s (%s)", a.Address, runtime)
	}
	return a.Address
}
