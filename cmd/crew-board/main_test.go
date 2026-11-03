package main

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"crew-services/internal/board"
	"crew-services/internal/board/boardsqlite"
	"crew-services/internal/humans"
	"crew-services/internal/humans/humanshttp"
	"crew-services/internal/humans/humanssqlite"
	"crew-services/internal/humansclient"
)

type nobody struct{}

func (nobody) Session(context.Context, string) (string, bool, error)    { return "", false, nil }
func (nobody) SeatHolder(context.Context, string) (string, bool, error) { return "", false, nil }

// A long post that mentions a person reaches crew-humans through its real
// HTTP API with a short excerpt, instead of being refused for its size.
func TestALongPostStillNotifiesAPerson(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	hstore, err := humanssqlite.Open(ctx, filepath.Join(dir, "humans.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer hstore.Close()
	hsvc, err := humans.New(hstore, time.Now, humans.Config{People: humans.PeopleFile{Site: "den", People: []humans.Person{{Name: "patch"}, {Name: "lore"}}}})
	if err != nil {
		t.Fatal(err)
	}
	api := httptest.NewServer(humanshttp.NewHandler(hsvc))
	defer api.Close()
	client, _ := humansclient.New(api.URL)

	store, err := boardsqlite.Open(ctx, filepath.Join(dir, "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc, err := board.New(store, nobody{}, time.Now, board.Config{Site: "den", People: []string{"patch", "lore"}, PeopleRouted: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.CreateBoard(ctx, board.CreateBoardRequest{Actor: board.Actor{Person: "lore"}, OperationID: "b", Name: "design"}); err != nil {
		t.Fatal(err)
	}
	body := "@patch " + strings.Repeat("plenty of ordinary unfolded prose here ", 2000)
	if len(body) < 70<<10 {
		t.Fatalf("the post is only %d bytes", len(body))
	}
	if _, _, err := svc.OpenThread(ctx, board.OpenThreadRequest{Actor: board.Actor{Person: "lore"}, OperationID: "t", Board: "design", Title: "Long", Body: body}); err != nil {
		t.Fatal(err)
	}
	n := board.NewNotifier(store, nobody{}, nil, time.Now, board.NotifierConfig{Site: "den", People: peopleNotifier{client}, WebURL: "https://board.dragonden.stream"})
	n.Pass(ctx)
	notices, err := svc.Notices(ctx, "", 10)
	if err != nil || len(notices) != 1 || notices[0].State != board.NoticeDelivered {
		t.Fatalf("board notices = %+v %v", notices, err)
	}
	inbox, err := hsvc.Notices(ctx, "patch", false, "", 10)
	if err != nil || len(inbox.Notices) != 1 || len([]rune(inbox.Notices[0].Text)) > 600 || !strings.HasPrefix(inbox.Notices[0].Text, "@patch plenty") {
		t.Fatalf("crew-humans has %+v %v", inbox, err)
	}
}
