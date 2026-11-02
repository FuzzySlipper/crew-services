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
		page, err := svc.ListThreads(r.Context(), r.PathValue("board"), r.URL.Query().Get("after"), limit(r))
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
	return mux
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
	case board.CodeBoardExists, board.CodeOperationConflict, board.CodePostNotVisible:
		status = http.StatusConflict
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
