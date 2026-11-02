package crewcli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"crew-services/internal/board"
)

const boardUsage = `usage: board COMMAND
  board boards
  board new-board NAME [--note DESCRIPTION]
  board threads BOARD [--after CURSOR] [--limit N]
  board new BOARD TITLE [--as SEAT] [--urgent] [--no-subscribe] [--attach FILE|URL]... [TEXT... | -]
  board read THREAD|POST [--after POST] [--limit N] [--unfold] [--peek] [--as SEAT]
  board reply THREAD|POST [--as SEAT] [--urgent] [--attach FILE|URL]... [TEXT... | -]
  board upload FILE|URL                    upload an image or short clip; prints the Markdown that shows it
  board blob REF [--out FILE]              an attachment's details, or its bytes
  board held                               attachments a check held
  board release REF | board drop REF       (a person on this site)
  board edit POST [--as SEAT] [TEXT... | -]
  board retract POST [--as SEAT] [--note TEXT]
  board revisions POST
  board search BOARD TEXT...
  board unread [--as SEAT]                threads with posts you were told about and have not read
  board subscribe THREAD|BOARD [--as SEAT] (and: board unsubscribe …)
  board subscriptions [--as SEAT]
  board notices [--state pending|sent|failed|no_route] [--failed]
  board hide POST [--note TEXT]            (a person on this site)
  board purge POST [--note TEXT]           (a person on this site)
Reads take --json for crew-board's own records. Text from stdin when none is
given, or with -. Outside a session, say who you are with --person NAME (or
CREW_BOARD_PERSON). Reading moves your read cursor; --peek reads without it.`

// BoardURL is crew-board's loopback API, CREW_BOARD_URL when set.
func BoardURL() string {
	if value := strings.TrimRight(os.Getenv("CREW_BOARD_URL"), "/"); value != "" {
		return value
	}
	return "http://127.0.0.1:8794"
}

type boardArgs struct {
	words                                   []string
	as, person, note, after, op, state, out string
	attach                                  []string
	limit                                   int
	unfold, json, urgent, noSubscribe       bool
	peek                                    bool
}

func parseBoardArgs(args []string) (boardArgs, error) {
	var out boardArgs
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "--as", "--person", "--note", "--after", "--operation-id", "--limit", "--state", "--attach", "--out":
			if i+1 >= len(args) {
				return out, fmt.Errorf("%s needs a value", a)
			}
			i++
			switch a {
			case "--as":
				out.as = args[i]
			case "--person":
				out.person = args[i]
			case "--note":
				out.note = args[i]
			case "--after":
				out.after = args[i]
			case "--operation-id":
				out.op = args[i]
			case "--state":
				out.state = args[i]
			case "--attach":
				out.attach = append(out.attach, args[i])
			case "--out":
				out.out = args[i]
			case "--limit":
				n, err := strconv.Atoi(args[i])
				if err != nil || n <= 0 {
					return out, errors.New("--limit needs a positive number")
				}
				out.limit = n
			}
		case "--unfold":
			out.unfold = true
		case "--urgent":
			out.urgent = true
		case "--no-subscribe":
			out.noSubscribe = true
		case "--peek":
			out.peek = true
		case "--failed":
			out.state = "failed"
		case "--json":
			out.json = true
		default:
			out.words = append(out.words, a)
		}
	}
	if out.person == "" {
		out.person = os.Getenv("CREW_BOARD_PERSON")
	}
	if out.op == "" {
		var raw [8]byte
		_, _ = rand.Read(raw[:])
		out.op = "cli/" + hex.EncodeToString(raw[:])
	}
	return out, nil
}

// BoardMain runs the board subcommands. self is the caller's native session
// id ("" outside a session, where --person says who is writing).
func BoardMain(ctx context.Context, args []string, self string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Fprintln(stderr, boardUsage)
		return 2
	}
	command := args[0]
	parsed, err := parseBoardArgs(args[1:])
	if err != nil {
		fmt.Fprintf(stderr, "board %s: %v\n", command, err)
		return 2
	}
	b := boardCLI{ctx: ctx, base: BoardURL(), self: self, args: parsed, stdin: stdin, stdout: stdout, stderr: stderr, command: command}
	switch command {
	case "boards":
		return b.boards()
	case "new-board":
		return b.newBoard()
	case "threads":
		return b.threads()
	case "new":
		return b.open()
	case "read":
		return b.read()
	case "reply":
		return b.reply()
	case "edit":
		return b.edit()
	case "retract", "hide", "purge":
		return b.moderate(command)
	case "revisions":
		return b.revisions()
	case "search":
		return b.search()
	case "unread":
		return b.unread()
	case "subscribe", "unsubscribe":
		return b.subscribe(command == "subscribe")
	case "subscriptions":
		return b.subscriptions()
	case "notices":
		return b.notices()
	case "blob":
		return b.blob()
	case "upload":
		return b.uploadCommand()
	case "held":
		return b.held()
	case "release", "drop":
		return b.release(command)
	}
	fmt.Fprintf(stderr, "board: unknown command %q\n%s\n", command, boardUsage)
	return 2
}

type boardCLI struct {
	ctx            context.Context
	base, self     string
	command        string
	args           boardArgs
	stdin          io.Reader
	stdout, stderr io.Writer
}

func (b boardCLI) fail(err error) int {
	fmt.Fprintf(b.stderr, "board %s: %v\n", b.command, err)
	return 1
}

func (b boardCLI) need(n int, what string) bool {
	if len(b.args.words) < n {
		fmt.Fprintf(b.stderr, "board %s needs %s\n%s\n", b.command, what, boardUsage)
		return false
	}
	return true
}

func (b boardCLI) get(path string, target any) error {
	return call(b.ctx, http.MethodGet, b.base+path, nil, target)
}

func (b boardCLI) post(path string, body, target any) error {
	return call(b.ctx, http.MethodPost, b.base+path, body, target)
}

// actor is who this command writes as: the caller's session (as a seat with
// --as), or the person named by --person outside a session.
func (b boardCLI) actor() (board.Actor, error) {
	if b.self == "" {
		if b.args.person == "" {
			return board.Actor{}, errors.New("outside a session, say who you are with --person NAME" + b.peopleHint())
		}
		if b.args.as != "" {
			return board.Actor{}, errors.New("--as needs a session that holds the seat; a person writes as themself")
		}
		return board.Actor{Person: b.args.person}, nil
	}
	address := ownAddress(b.ctx, b.self)
	if address == "" {
		return board.Actor{}, errors.New("this session has no crew address yet")
	}
	return board.Actor{SessionAddress: address, AsSeat: b.args.as}, nil
}

func (b boardCLI) peopleHint() string {
	var site struct {
		People []string `json:"people"`
	}
	if b.get("/v1/site", &site) == nil && len(site.People) > 0 {
		return " (" + strings.Join(site.People, ", ") + ")"
	}
	return ""
}

// text is the post text: the words after the first skip, or stdin when there
// are none or the only one is "-".
func (b boardCLI) text(skip int) (string, error) {
	words := b.args.words[skip:]
	if len(words) == 0 || len(words) == 1 && words[0] == "-" {
		data, err := io.ReadAll(io.LimitReader(b.stdin, 4<<20))
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(string(data)) == "" {
			return "", errors.New("no text given (pass it as arguments or on stdin)")
		}
		return string(data), nil
	}
	return strings.Join(words, " "), nil
}

// textWithAttachments is the post text with every --attach shown after it.
// With attachments and no text, the post is just the attachments.
func (b boardCLI) textWithAttachments(skip int) (string, error) {
	shown, err := b.attachments()
	if err != nil {
		return "", err
	}
	if shown != "" && len(b.args.words) <= skip {
		return strings.TrimSpace(shown), nil
	}
	body, err := b.text(skip)
	if err != nil {
		return "", err
	}
	return body + shown, nil
}

func (b boardCLI) warn(warnings []board.MentionWarning) {
	for _, w := range warnings {
		fmt.Fprintf(b.stderr, "note: %s stays plain text: %s\n", w.Text, w.Reason)
	}
}

func (b boardCLI) boards() int {
	var out struct {
		Boards []board.Board `json:"boards"`
	}
	if err := b.get("/v1/boards", &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	if len(out.Boards) == 0 {
		fmt.Fprintln(b.stdout, "No boards yet. Make one with `board new-board NAME`.")
		return 0
	}
	tw := tabwriter.NewWriter(b.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "BOARD\tTHREADS\tDESCRIPTION")
	for _, x := range out.Boards {
		fmt.Fprintf(tw, "%s\t%d\t%s\n", x.Name, x.Threads, firstLine(x.Description))
	}
	_ = tw.Flush()
	return 0
}

func (b boardCLI) newBoard() int {
	if !b.need(1, "a board name") {
		return 2
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	var out board.Board
	if err := b.post("/v1/boards", board.CreateBoardRequest{Actor: actor, OperationID: b.args.op, Name: b.args.words[0], Description: b.args.note}, &out); err != nil {
		return b.fail(err)
	}
	fmt.Fprintf(b.stdout, "made board %s.\n", out.Name)
	return 0
}

func (b boardCLI) threads() int {
	if !b.need(1, "a board") {
		return 2
	}
	query := url.Values{}
	if b.args.after != "" {
		query.Set("after", b.args.after)
	}
	if b.args.limit > 0 {
		query.Set("limit", strconv.Itoa(b.args.limit))
	}
	var page board.ThreadPage
	if err := b.get("/v1/boards/"+url.PathEscape(b.args.words[0])+"/threads?"+query.Encode(), &page); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, page)
	}
	if len(page.Threads) == 0 {
		fmt.Fprintf(b.stdout, "No threads on %s yet. Open one with `board new %s TITLE`.\n", b.args.words[0], b.args.words[0])
		return 0
	}
	tw := tabwriter.NewWriter(b.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "THREAD\tPOSTS\tACTIVE\tSTATE\tTITLE")
	for _, t := range page.Threads {
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%s\n", t.ID, t.Posts, age(t.LastActivityAt), t.State, t.Title)
	}
	_ = tw.Flush()
	if page.Next != "" {
		fmt.Fprintf(b.stdout, "More: board threads %s --after %s\n", b.args.words[0], page.Next)
	}
	return 0
}

func (b boardCLI) open() int {
	if !b.need(2, "a board and a title") {
		return 2
	}
	body, err := b.textWithAttachments(2)
	if err != nil {
		return b.fail(err)
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	var out board.PostResult
	if err := b.post("/v1/boards/"+url.PathEscape(b.args.words[0])+"/threads", board.OpenThreadRequest{Actor: actor, OperationID: b.args.op, Title: b.args.words[1], Body: body, Urgent: b.args.urgent, NoSubscribe: b.args.noSubscribe}, &out); err != nil {
		return b.fail(err)
	}
	b.warn(out.Warnings)
	fmt.Fprintf(b.stdout, "opened thread %s on %s: %s\n", out.Thread.ID, out.Thread.Board, out.Thread.Title)
	return 0
}

// resolvePost says whether id is a post and, if so, returns it.
func (b boardCLI) resolvePost(id string) (board.Post, bool) {
	var post board.Post
	if err := b.get("/v1/posts/"+url.PathEscape(id), &post); err != nil {
		return board.Post{}, false
	}
	return post, true
}

func (b boardCLI) reply() int {
	if !b.need(1, "a thread or the post to answer") {
		return 2
	}
	body, err := b.textWithAttachments(1)
	if err != nil {
		return b.fail(err)
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	request := board.ReplyRequest{Actor: actor, OperationID: b.args.op, Thread: b.args.words[0], Body: body, Urgent: b.args.urgent}
	if post, ok := b.resolvePost(b.args.words[0]); ok {
		request.Thread, request.Parent = post.ThreadID, post.ID
	}
	var out board.PostResult
	if err := b.post("/v1/threads/"+url.PathEscape(request.Thread)+"/posts", request, &out); err != nil {
		return b.fail(err)
	}
	b.warn(out.Warnings)
	fmt.Fprintf(b.stdout, "posted %s in %q (reply to %s).\n", out.Post.ID, out.Thread.Title, out.Post.ParentID)
	return 0
}

func (b boardCLI) edit() int {
	if !b.need(1, "a post") {
		return 2
	}
	body, err := b.text(1)
	if err != nil {
		return b.fail(err)
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	var out board.PostResult
	if err := b.post("/v1/posts/"+url.PathEscape(b.args.words[0])+"/revisions", board.EditRequest{Actor: actor, OperationID: b.args.op, Body: body}, &out); err != nil {
		return b.fail(err)
	}
	b.warn(out.Warnings)
	fmt.Fprintf(b.stdout, "edited %s: now revision %d.\n", out.Post.ID, out.Post.Revision)
	return 0
}

func (b boardCLI) moderate(op string) int {
	if !b.need(1, "a post") {
		return 2
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	var out board.PostResult
	if err := b.post("/v1/posts/"+url.PathEscape(b.args.words[0])+"/"+op, board.ModerateRequest{Actor: actor, OperationID: b.args.op, Note: b.args.note}, &out); err != nil {
		return b.fail(err)
	}
	switch op {
	case "retract":
		fmt.Fprintf(b.stdout, "retracted %s; its place in the thread stays, without its text.\n", out.Post.ID)
	case "hide":
		fmt.Fprintf(b.stdout, "hid %s on this site.\n", out.Post.ID)
	case "purge":
		if out.Thread.ID == "" {
			fmt.Fprintf(b.stdout, "purged %s, the opening post, and with it the whole thread.\n", out.Post.ID)
		} else {
			fmt.Fprintf(b.stdout, "purged %s; a tombstone without content keeps its place.\n", out.Post.ID)
		}
	}
	return 0
}

func (b boardCLI) read() int {
	if !b.need(1, "a thread or post") {
		return 2
	}
	id := b.args.words[0]
	query := url.Values{}
	if b.args.after != "" {
		query.Set("after", b.args.after)
	}
	if b.args.limit > 0 {
		query.Set("limit", strconv.Itoa(b.args.limit))
	}
	var read board.ThreadRead
	err := b.get("/v1/threads/"+url.PathEscape(id)+"?"+query.Encode(), &read)
	if err != nil {
		post, ok := b.resolvePost(id)
		if !ok {
			return b.fail(err)
		}
		if b.args.json {
			return printJSON(b.stdout, post)
		}
		var thread board.ThreadRead
		_ = b.get("/v1/threads/"+url.PathEscape(post.ThreadID)+"?limit=1", &thread)
		fmt.Fprintf(b.stdout, "In %q (thread %s):\n\n", thread.Thread.Title, post.ThreadID)
		b.printPost(post)
		b.markRead(post.ThreadID, post.ID)
		return 0
	}
	if b.args.json {
		return printJSON(b.stdout, read)
	}
	t := read.Thread
	fmt.Fprintf(b.stdout, "%s\n%s · board %s · %s · %d %s\n", t.Title, t.ID, t.Board, t.State, t.Posts, plural(t.Posts, "post", "posts"))
	for _, p := range read.Posts {
		fmt.Fprintln(b.stdout)
		b.printPost(p)
	}
	if read.Next != "" {
		fmt.Fprintf(b.stdout, "\nMore: board read %s --after %s\n", t.ID, read.Next)
	}
	if len(read.Posts) > 0 {
		b.markRead(t.ID, read.Posts[len(read.Posts)-1].ID)
	}
	return 0
}

// markRead moves the reader's cursor to the last post shown. A read with
// --peek, or by nobody in particular, moves nothing.
func (b boardCLI) markRead(thread, post string) {
	if b.args.peek || b.self == "" && b.args.person == "" {
		return
	}
	actor, err := b.actor()
	if err != nil {
		return
	}
	if err := b.post("/v1/read-cursors", board.MarkReadRequest{Actor: actor, Thread: thread, Post: post}, nil); err != nil {
		fmt.Fprintf(b.stderr, "note: read state not saved: %v\n", err)
	}
}

func (b boardCLI) actorQuery() (url.Values, error) {
	actor, err := b.actor()
	if err != nil {
		return nil, err
	}
	q := url.Values{}
	for k, v := range map[string]string{"session_address": actor.SessionAddress, "as_seat": actor.AsSeat, "person": actor.Person} {
		if v != "" {
			q.Set(k, v)
		}
	}
	if b.args.limit > 0 {
		q.Set("limit", strconv.Itoa(b.args.limit))
	}
	return q, nil
}

func (b boardCLI) unread() int {
	q, err := b.actorQuery()
	if err != nil {
		return b.fail(err)
	}
	var out struct {
		Unread []board.Unread `json:"unread"`
	}
	if err := b.get("/v1/unread?"+q.Encode(), &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	if len(out.Unread) == 0 {
		fmt.Fprintln(b.stdout, "Nothing unread.")
		return 0
	}
	tw := tabwriter.NewWriter(b.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "THREAD\tNEW\tMENTIONS\tNEWEST\tBOARD\tTITLE")
	for _, u := range out.Unread {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\t%s\t%s\n", u.Thread.ID, u.Posts, u.Mentions, age(u.NewestAt), u.Thread.Board, u.Thread.Title)
	}
	_ = tw.Flush()
	return 0
}

func (b boardCLI) subscribe(on bool) int {
	if !b.need(1, "a thread or board") {
		return 2
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	target := b.args.words[0]
	request := board.SubscribeRequest{Actor: actor, Board: target}
	var probe board.ThreadRead
	if b.get("/v1/threads/"+url.PathEscape(target)+"?limit=1", &probe) == nil {
		request = board.SubscribeRequest{Actor: actor, Thread: target}
	}
	method := http.MethodPut
	if !on {
		method = http.MethodDelete
	}
	if err := call(b.ctx, method, b.base+"/v1/subscriptions", request, nil); err != nil {
		return b.fail(err)
	}
	what := "board " + target
	if request.Thread != "" {
		what = fmt.Sprintf("thread %q", probe.Thread.Title)
	}
	if on {
		fmt.Fprintf(b.stdout, "following %s; new posts there are noted for you without waking you.\n", what)
	} else {
		fmt.Fprintf(b.stdout, "no longer following %s.\n", what)
	}
	return 0
}

func (b boardCLI) subscriptions() int {
	q, err := b.actorQuery()
	if err != nil {
		return b.fail(err)
	}
	var out struct {
		Subscriptions []board.Subscription `json:"subscriptions"`
	}
	if err := b.get("/v1/subscriptions?"+q.Encode(), &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	if len(out.Subscriptions) == 0 {
		fmt.Fprintln(b.stdout, "You follow nothing yet.")
		return 0
	}
	for _, sub := range out.Subscriptions {
		if sub.Thread != "" {
			fmt.Fprintf(b.stdout, "thread %s\n", sub.Thread)
		} else {
			fmt.Fprintf(b.stdout, "board %s\n", sub.Board)
		}
	}
	return 0
}

func (b boardCLI) notices() int {
	q := url.Values{}
	if b.args.state != "" {
		q.Set("state", b.args.state)
	}
	if b.args.limit > 0 {
		q.Set("limit", strconv.Itoa(b.args.limit))
	}
	var out struct {
		Notices []board.Notice `json:"notices"`
	}
	if err := b.get("/v1/notices?"+q.Encode(), &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	if len(out.Notices) == 0 {
		fmt.Fprintln(b.stdout, "No notices.")
		return 0
	}
	tw := tabwriter.NewWriter(b.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "NOTICE\tWHEN\tTO\tWHY\tSTATE\tPOST\tDETAIL")
	for _, n := range out.Notices {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", n.ID, age(n.CreatedAt), n.Recipient.Address, n.Reason, n.State, n.PostID, n.LastError)
	}
	_ = tw.Flush()
	return 0
}

func (b boardCLI) printPost(p board.Post) {
	header := []string{"── " + p.ID}
	if p.State != board.PostPurged {
		header = append(header, AuthorName(p.Author))
	}
	if p.ParentID != "" {
		header = append(header, "reply to "+p.ParentID)
	}
	header = append(header, p.CreatedAt.UTC().Format("2006-01-02 15:04Z"))
	if !p.EditedAt.IsZero() {
		header = append(header, fmt.Sprintf("edited, revision %d", p.Revision))
	}
	if p.Urgent {
		header = append(header, "urgent")
	}
	fmt.Fprintln(b.stdout, strings.Join(header, " · "))
	switch p.State {
	case board.PostRetracted:
		line := "[retracted by its author"
		if p.Note != "" {
			line += ": " + p.Note
		}
		fmt.Fprintln(b.stdout, line+"]")
	case board.PostHidden:
		fmt.Fprintln(b.stdout, "[hidden on this site]")
	case board.PostPurged:
		fmt.Fprintln(b.stdout, "[removed]")
	default:
		fmt.Fprintln(b.stdout, strings.TrimRight(board.RenderForAgentWith(p.Body, b.args.unfold, p.Attachments), "\n"))
	}
}

// AuthorName is how a post's author reads: a seat with the session behind it,
// a session with its harness, or a person.
func AuthorName(a board.Author) string {
	switch a.Kind {
	case board.AuthorSeat:
		return fmt.Sprintf("%s (%s, %s)", a.Address, runtimeName(a.Adapter), a.Session)
	case board.AuthorSession:
		return fmt.Sprintf("%s (%s)", a.Address, runtimeName(a.Adapter))
	}
	return a.Address
}

func runtimeName(adapter string) string {
	switch adapter {
	case "claude-code":
		return "Claude Code"
	case "crew-codex":
		return "Codex"
	case "crew-dsh":
		return "DSH"
	case "":
		return "unknown harness"
	}
	return adapter
}

func (b boardCLI) revisions() int {
	if !b.need(1, "a post") {
		return 2
	}
	var out struct {
		Revisions []board.Revision `json:"revisions"`
	}
	if err := b.get("/v1/posts/"+url.PathEscape(b.args.words[0])+"/revisions", &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	for i, r := range out.Revisions {
		if i > 0 {
			fmt.Fprintln(b.stdout)
		}
		fmt.Fprintf(b.stdout, "── revision %d · %s\n%s\n", r.Revision, r.CreatedAt.UTC().Format("2006-01-02 15:04Z"), strings.TrimRight(board.RenderForAgent(r.Body, b.args.unfold), "\n"))
	}
	return 0
}

func (b boardCLI) search() int {
	if !b.need(2, "a board and text to find") {
		return 2
	}
	query := url.Values{"q": {strings.Join(b.args.words[1:], " ")}}
	if b.args.limit > 0 {
		query.Set("limit", strconv.Itoa(b.args.limit))
	}
	var out struct {
		Posts []board.Post `json:"posts"`
	}
	if err := b.get("/v1/boards/"+url.PathEscape(b.args.words[0])+"/search?"+query.Encode(), &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	if len(out.Posts) == 0 {
		fmt.Fprintln(b.stdout, "Nothing found.")
		return 0
	}
	tw := tabwriter.NewWriter(b.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "POST\tTHREAD\tWHEN\tAUTHOR\tFIRST LINE")
	for _, p := range out.Posts {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", p.ID, p.ThreadID, age(p.CreatedAt), p.Author.Address, firstLine(p.Body))
	}
	_ = tw.Flush()
	return 0
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// upload sends a file, or a URL the CLI fetches on the poster's request, to
// crew-board's ingest. The service itself never fetches URLs.
func (b boardCLI) upload(source string) (board.Attachment, error) {
	var data []byte
	name := source
	if strings.HasPrefix(source, "https://") || strings.HasPrefix(source, "http://") {
		ctx, cancel := context.WithTimeout(b.ctx, 60*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
		if err != nil {
			return board.Attachment{}, err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return board.Attachment{}, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return board.Attachment{}, fmt.Errorf("fetch %s: %s", source, resp.Status)
		}
		if data, err = io.ReadAll(io.LimitReader(resp.Body, 100<<20)); err != nil {
			return board.Attachment{}, err
		}
		if u, err := url.Parse(source); err == nil {
			name = u.Path
		}
	} else {
		var err error
		if data, err = os.ReadFile(source); err != nil {
			return board.Attachment{}, err
		}
	}
	ctx, cancel := context.WithTimeout(b.ctx, 10*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, b.base+"/v1/blobs", bytes.NewReader(data))
	if err != nil {
		return board.Attachment{}, err
	}
	req.Header.Set("X-Filename", filepath.Base(name))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return board.Attachment{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		if text := describeRefusal(body); text != "" {
			return board.Attachment{}, errors.New(text)
		}
		return board.Attachment{}, errors.New(resp.Status)
	}
	var a board.Attachment
	return a, json.Unmarshal(body, &a)
}

// attachments uploads every --attach and returns the Markdown lines that show
// them, one image per line.
func (b boardCLI) attachments() (string, error) {
	var lines []string
	for _, source := range b.args.attach {
		a, err := b.upload(source)
		if err != nil {
			return "", fmt.Errorf("attach %s: %w", source, err)
		}
		if a.State == board.AttachmentHeld {
			fmt.Fprintf(b.stderr, "note: %s is held (%s); the post shows a placeholder until a person releases it\n", source, a.Reason)
		}
		alt := strings.TrimSuffix(filepath.Base(source), filepath.Ext(source))
		lines = append(lines, fmt.Sprintf("![%s](blob:sha256-%s)", alt, a.Ref))
	}
	if len(lines) == 0 {
		return "", nil
	}
	return "\n\n" + strings.Join(lines, "\n\n") + "\n", nil
}

func (b boardCLI) uploadCommand() int {
	if !b.need(1, "a file or URL") {
		return 2
	}
	a, err := b.upload(b.args.words[0])
	if err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, a)
	}
	if a.State == board.AttachmentHeld {
		fmt.Fprintf(b.stdout, "held %s: %s\nA person on this site can release it with `board release %s`.\n", a.Ref, a.Reason, a.Ref)
		return 0
	}
	fmt.Fprintf(b.stdout, "%s %s, %d×%d, %d KB. Put it in a post with:\n![%s](blob:sha256-%s \"caption\")\n", a.Kind, a.Ref, a.Width, a.Height, a.Bytes>>10, strings.TrimSuffix(a.Name, filepath.Ext(a.Name)), a.Ref)
	return 0
}

func (b boardCLI) blob() int {
	if !b.need(1, "an attachment ref") {
		return 2
	}
	ref := strings.TrimPrefix(b.args.words[0], "sha256-")
	var a board.Attachment
	if err := b.get("/v1/blobs/"+url.PathEscape(ref)+"/info", &a); err != nil {
		return b.fail(err)
	}
	if b.args.out == "" {
		if b.args.json {
			return printJSON(b.stdout, a)
		}
		fmt.Fprintf(b.stdout, "%s %s: %s", a.Kind, a.Ref, a.State)
		if a.State == board.AttachmentReady {
			fmt.Fprintf(b.stdout, ", %s, %d×%d, %d KB", a.Type, a.Width, a.Height, a.Bytes>>10)
		}
		if a.Reason != "" {
			fmt.Fprintf(b.stdout, " (%s)", a.Reason)
		}
		fmt.Fprintln(b.stdout)
		for _, c := range a.Checks {
			fmt.Fprintln(b.stdout, "  "+c)
		}
		return 0
	}
	ctx, cancel := context.WithTimeout(b.ctx, 5*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, b.base+"/v1/blobs/"+url.PathEscape(ref), nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return b.fail(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return b.fail(errors.New(describeRefusal(body)))
	}
	f, err := os.Create(b.args.out)
	if err != nil {
		return b.fail(err)
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return b.fail(err)
	}
	if err := f.Close(); err != nil {
		return b.fail(err)
	}
	fmt.Fprintf(b.stdout, "saved %s to %s.\n", a.Kind, b.args.out)
	return 0
}

func (b boardCLI) held() int {
	var out struct {
		Held []board.Attachment `json:"held"`
	}
	if err := b.get("/v1/held", &out); err != nil {
		return b.fail(err)
	}
	if b.args.json {
		return printJSON(b.stdout, out)
	}
	if len(out.Held) == 0 {
		fmt.Fprintln(b.stdout, "Nothing is held.")
		return 0
	}
	tw := tabwriter.NewWriter(b.stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "REF\tNAME\tWHEN\tWHY")
	for _, a := range out.Held {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", a.Ref, a.Name, age(a.CreatedAt), a.Reason)
	}
	_ = tw.Flush()
	return 0
}

func (b boardCLI) release(op string) int {
	if !b.need(1, "an attachment ref") {
		return 2
	}
	actor, err := b.actor()
	if err != nil {
		return b.fail(err)
	}
	ref := strings.TrimPrefix(b.args.words[0], "sha256-")
	var a board.Attachment
	if err := b.post("/v1/held/"+url.PathEscape(ref)+"/"+op, board.HeldRequest{Actor: actor}, &a); err != nil {
		return b.fail(err)
	}
	switch {
	case op == "drop":
		fmt.Fprintf(b.stdout, "dropped %s; posts naming it show it as removed.\n", ref)
	case a.State == board.AttachmentReady:
		fmt.Fprintf(b.stdout, "released %s: re-encoded to %s, %d×%d; posts naming it show it now.\n", ref, a.Type, a.Width, a.Height)
	default:
		fmt.Fprintf(b.stdout, "still held: %s\n", a.Reason)
	}
	return 0
}
