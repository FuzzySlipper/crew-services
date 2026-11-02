// Package boardhttp is crew-board's loopback HTTP API: a thin layer over
// board.Service.
package boardhttp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"crew-services/internal/board"
)

// NewHandler serves crew-board's API (docs/crew-board.md, Operations).
func NewHandler(svc *board.Service) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	mux.HandleFunc("GET /v1/site", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"site": svc.Site(), "people": svc.People()})
	})
	mux.HandleFunc("GET /v1/boards", func(w http.ResponseWriter, r *http.Request) {
		boards, err := svc.ListBoards(r.Context())
		reply(w, map[string]any{"boards": boards}, err)
	})
	mux.HandleFunc("POST /v1/boards", write(func(ctx context.Context, request board.CreateBoardRequest, _ *http.Request) (any, bool, error) {
		return svc.CreateBoard(ctx, request)
	}))
	mux.HandleFunc("GET /v1/boards/{board}", func(w http.ResponseWriter, r *http.Request) {
		value, err := svc.GetBoard(r.Context(), r.PathValue("board"))
		reply(w, value, err)
	})
	mux.HandleFunc("GET /v1/boards/{board}/threads", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		var states []string
		for _, state := range strings.Split(q.Get("state"), ",") {
			if state = strings.TrimSpace(state); state != "" && state != "all" {
				states = append(states, state)
			}
		}
		page, err := svc.ListThreads(r.Context(), r.PathValue("board"), q.Get("after"), limit(r), states, q.Get("archived") == "true")
		reply(w, page, err)
	})
	mux.HandleFunc("POST /v1/boards/{board}/threads", write(func(ctx context.Context, request board.OpenThreadRequest, r *http.Request) (any, bool, error) {
		request.Board = r.PathValue("board")
		return svc.OpenThread(ctx, request)
	}))
	mux.HandleFunc("GET /v1/boards/{board}/search", func(w http.ResponseWriter, r *http.Request) {
		posts, err := svc.Search(r.Context(), r.PathValue("board"), r.URL.Query().Get("q"), limit(r))
		reply(w, map[string]any{"posts": posts}, err)
	})
	mux.HandleFunc("GET /v1/threads/{thread}", func(w http.ResponseWriter, r *http.Request) {
		read, err := svc.ReadThread(r.Context(), r.PathValue("thread"), r.URL.Query().Get("after"), limit(r))
		reply(w, read, err)
	})
	mux.HandleFunc("POST /v1/threads/{thread}/posts", write(func(ctx context.Context, request board.ReplyRequest, r *http.Request) (any, bool, error) {
		request.Thread = r.PathValue("thread")
		return svc.Reply(ctx, request)
	}))
	mux.HandleFunc("GET /v1/posts/{post}", func(w http.ResponseWriter, r *http.Request) {
		post, err := svc.GetPost(r.Context(), r.PathValue("post"))
		reply(w, post, err)
	})
	mux.HandleFunc("GET /v1/posts/{post}/revisions", func(w http.ResponseWriter, r *http.Request) {
		revisions, err := svc.Revisions(r.Context(), r.PathValue("post"))
		reply(w, map[string]any{"revisions": revisions}, err)
	})
	mux.HandleFunc("POST /v1/posts/{post}/revisions", write(func(ctx context.Context, request board.EditRequest, r *http.Request) (any, bool, error) {
		request.Post = r.PathValue("post")
		return svc.Edit(ctx, request)
	}))
	for name, op := range map[string]func(context.Context, board.ModerateRequest) (board.PostResult, bool, error){"retract": svc.Retract, "hide": svc.Hide, "purge": svc.Purge} {
		mux.HandleFunc("POST /v1/posts/{post}/"+name, write(func(ctx context.Context, request board.ModerateRequest, r *http.Request) (any, bool, error) {
			request.Post = r.PathValue("post")
			return op(ctx, request)
		}))
	}
	mux.HandleFunc("PUT /v1/subscriptions", plain(svc.Subscribe))
	mux.HandleFunc("DELETE /v1/subscriptions", plain(svc.Unsubscribe))
	mux.HandleFunc("GET /v1/subscriptions", func(w http.ResponseWriter, r *http.Request) {
		subs, err := svc.Subscriptions(r.Context(), actorFrom(r))
		reply(w, map[string]any{"subscriptions": subs}, err)
	})
	mux.HandleFunc("POST /v1/read-cursors", plain(func(ctx context.Context, request board.MarkReadRequest) (map[string]string, error) {
		return map[string]string{"thread": request.Thread, "post": request.Post}, svc.MarkRead(ctx, request)
	}))
	mux.HandleFunc("GET /v1/unread", func(w http.ResponseWriter, r *http.Request) {
		unread, err := svc.Unread(r.Context(), actorFrom(r), limit(r))
		reply(w, map[string]any{"unread": unread}, err)
	})
	mux.HandleFunc("GET /v1/notices", func(w http.ResponseWriter, r *http.Request) {
		notices, err := svc.Notices(r.Context(), r.URL.Query().Get("state"), limit(r))
		reply(w, map[string]any{"notices": notices}, err)
	})
	mux.HandleFunc("POST /v1/blobs", func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(io.LimitReader(r.Body, (100<<20)+1))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": string(board.CodeInvalid), "error": "unreadable upload"})
			return
		}
		a, err := svc.Upload(r.Context(), r.Header.Get("X-Filename"), data)
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, a)
	})
	mux.HandleFunc("GET /v1/blobs/{ref}", func(w http.ResponseWriter, r *http.Request) {
		a, path, err := svc.BlobPath(r.Context(), r.PathValue("ref"))
		if err != nil {
			writeError(w, err)
			return
		}
		w.Header().Set("Content-Type", a.Type)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeFile(w, r, path)
	})
	mux.HandleFunc("GET /v1/blobs/{ref}/info", func(w http.ResponseWriter, r *http.Request) {
		a, err := svc.Attachment(r.Context(), r.PathValue("ref"))
		reply(w, a, err)
	})
	mux.HandleFunc("GET /v1/held", func(w http.ResponseWriter, r *http.Request) {
		held, err := svc.Held(r.Context())
		reply(w, map[string]any{"held": held}, err)
	})
	for name, op := range map[string]func(context.Context, board.HeldRequest) (board.Attachment, error){"release": svc.Release, "drop": svc.Drop} {
		mux.HandleFunc("POST /v1/held/{ref}/"+name, func(w http.ResponseWriter, r *http.Request) {
			var request board.HeldRequest
			if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]string{"code": string(board.CodeInvalid), "error": "invalid JSON"})
				return
			}
			request.Ref = r.PathValue("ref")
			a, err := op(r.Context(), request)
			reply(w, a, err)
		})
	}
	for name, op := range map[string]func(context.Context, board.ThreadRequest) (board.Thread, bool, error){"settle": svc.Settle, "rename": svc.Rename, "close": svc.Close} {
		mux.HandleFunc("POST /v1/threads/{thread}/"+name, write(func(ctx context.Context, request board.ThreadRequest, r *http.Request) (any, bool, error) {
			request.Thread = r.PathValue("thread")
			return op(ctx, request)
		}))
	}
	mux.HandleFunc("POST /v1/posts/{post}/ack", func(w http.ResponseWriter, r *http.Request) {
		var request board.AckRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": string(board.CodeInvalid), "error": "invalid JSON"})
			return
		}
		request.Post = r.PathValue("post")
		d, err := svc.Ack(r.Context(), request)
		reply(w, d, err)
	})
	mux.HandleFunc("GET /v1/acks", func(w http.ResponseWriter, r *http.Request) {
		decisions, err := svc.OpenAcks(r.Context(), r.URL.Query().Get("board"), limit(r))
		reply(w, map[string]any{"decisions": decisions}, err)
	})
	mux.HandleFunc("GET /v1/boards/{board}/digest", func(w http.ResponseWriter, r *http.Request) {
		d, err := svc.Digest(r.Context(), r.PathValue("board"))
		reply(w, d, err)
	})
	return mux
}

// actorFrom reads a reader's identity from the query: session_address with
// as_seat, or person.
func actorFrom(r *http.Request) board.Actor {
	q := r.URL.Query()
	return board.Actor{SessionAddress: q.Get("session_address"), AsSeat: q.Get("as_seat"), Person: q.Get("person")}
}

// plain decodes a request that needs no operation receipt (it is idempotent
// by nature) and answers 200.
func plain[T any, R any](run func(context.Context, T) (R, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request T
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": string(board.CodeInvalid), "error": "invalid JSON"})
			return
		}
		result, err := run(r.Context(), request)
		reply(w, result, err)
	}
}

// write decodes a request, runs it, and answers 201 for a new write and 200
// for an exact replay.
func write[T any, R any](run func(context.Context, T, *http.Request) (R, bool, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var request T
		if err := json.NewDecoder(io.LimitReader(r.Body, 2<<20)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"code": string(board.CodeInvalid), "error": "invalid JSON"})
			return
		}
		result, replayed, err := run(r.Context(), request, r)
		if err != nil {
			writeError(w, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		writeJSON(w, status, result)
	}
}

func reply(w http.ResponseWriter, value any, err error) {
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, value)
}

func limit(r *http.Request) int {
	n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	return n
}

func writeError(w http.ResponseWriter, err error) {
	var typed *board.Error
	if !errors.As(err, &typed) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"code": "internal_error", "error": err.Error()})
		return
	}
	status := http.StatusBadRequest
	switch typed.Code {
	case board.CodeBoardNotFound, board.CodeThreadNotFound, board.CodePostNotFound:
		status = http.StatusNotFound
	case board.CodeNotHolder, board.CodeNotAuthor, board.CodePersonOnly, board.CodeSessionUnbound, board.CodeUnknownPerson:
		status = http.StatusForbidden
	case board.CodeBoardExists, board.CodeOperationConflict, board.CodePostNotVisible, board.CodeAttachmentHeld:
		status = http.StatusConflict
	case board.CodeAttachmentUnknown:
		status = http.StatusNotFound
	case board.CodeMediaTooLarge:
		status = http.StatusRequestEntityTooLarge
	case board.CodeMediaTypeRefused, board.CodeMediaTypeMismatch:
		status = http.StatusUnsupportedMediaType
	case board.CodeDirectoryUnreached:
		status = http.StatusBadGateway
	}
	body := map[string]any{"code": string(typed.Code)}
	if typed.Err != nil {
		body["error"] = typed.Err.Error()
	}
	if typed.Detail != nil {
		body["detail"] = typed.Detail
	}
	writeJSON(w, status, body)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
