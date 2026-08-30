package review

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
)

func NewHandler(s *Service) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if e := s.Ready(r.Context()); e != nil {
			write(w, 503, map[string]string{"status": "unavailable"})
			return
		}
		write(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("POST /v1/review-jobs", func(w http.ResponseWriter, r *http.Request) {
		var a Admission
		if !decode(w, r, &a) {
			return
		}
		j, replay, e := s.Admit(r.Context(), a)
		if e != nil {
			errJSON(w, e)
			return
		}
		status := 201
		if replay {
			status = 200
		}
		write(w, status, map[string]any{"job": j.Projection(), "replayed": replay})
	})
	m.HandleFunc("POST /v1/review-submissions", func(w http.ResponseWriter, r *http.Request) {
		var request SubmissionRequest
		if !decode(w, r, &request) {
			return
		}
		request.IdempotencyKey = r.Header.Get("Idempotency-Key")
		receipt, replayed, err := s.SubmitTaskForReview(r.Context(), request)
		if err != nil {
			errJSON(w, err)
			return
		}
		receipt.Replayed = replayed
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		}
		write(w, status, receipt)
	})
	m.HandleFunc("GET /v1/projects/{project_id}/tasks/{task_id}/manual-review", func(w http.ResponseWriter, r *http.Request) {
		taskID, err := strconv.ParseInt(r.PathValue("task_id"), 10, 64)
		if err != nil {
			errJSON(w, err)
			return
		}
		capability, err := s.GetManualReviewCapability(r.Context(), r.PathValue("project_id"), taskID)
		if err != nil {
			errJSON(w, err)
			return
		}
		write(w, http.StatusOK, capability)
	})
	m.HandleFunc("POST /v1/projects/{project_id}/tasks/{task_id}/manual-review", func(w http.ResponseWriter, r *http.Request) {
		taskID, err := strconv.ParseInt(r.PathValue("task_id"), 10, 64)
		if err != nil {
			errJSON(w, err)
			return
		}
		receipt, replayed, err := s.SubmitManualReview(r.Context(), ManualReviewSubmissionRequest{
			ProjectID:      r.PathValue("project_id"),
			TaskID:         taskID,
			IdempotencyKey: r.Header.Get("Idempotency-Key"),
			Reviewer:       r.Header.Get("X-Review-Actor"),
		})
		if err != nil {
			errJSON(w, err)
			return
		}
		status := http.StatusCreated
		if replayed {
			status = http.StatusOK
		} else if receipt.Status == "pending" {
			status = http.StatusAccepted
		}
		write(w, status, receipt)
	})
	m.HandleFunc("GET /v1/review-jobs/{id}", func(w http.ResponseWriter, r *http.Request) {
		j, e := s.Get(r.Context(), r.PathValue("id"))
		if e != nil {
			errJSON(w, e)
			return
		}
		write(w, 200, j.Projection())
	})
	m.HandleFunc("POST /v1/review-jobs/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		j, e := s.Cancel(r.Context(), r.PathValue("id"))
		if e != nil {
			errJSON(w, e)
			return
		}
		write(w, 200, j.Projection())
	})
	m.HandleFunc("DELETE /v1/review-affinities/{project}/{task}", func(w http.ResponseWriter, r *http.Request) {
		task, e := strconv.ParseInt(r.PathValue("task"), 10, 64)
		if e != nil {
			errJSON(w, e)
			return
		}
		if e = s.ReleaseAffinity(r.Context(), TaskKey{ProjectID: r.PathValue("project"), TaskID: task}); e != nil {
			errJSON(w, e)
			return
		}
		write(w, 200, map[string]bool{"released": true})
	})
	m.HandleFunc("GET /v1/review-pool", func(w http.ResponseWriter, r *http.Request) {
		n, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		v, e := s.Snapshot(r.Context(), n)
		if e != nil {
			errJSON(w, e)
			return
		}
		write(w, 200, v)
	})
	return m
}
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	defer r.Body.Close()
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if e := d.Decode(v); e != nil {
		errJSON(w, e)
		return false
	}
	if d.Decode(&struct{}{}) != io.EOF {
		errJSON(w, errors.New("one JSON object required"))
		return false
	}
	return true
}
func write(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func errJSON(w http.ResponseWriter, e error) {
	status := 400
	code := "invalid"
	if errors.Is(e, ErrNotFound) {
		status = 404
		code = "not_found"
	}
	if errors.Is(e, ErrConflict) || errors.Is(e, ErrAffinityBusy) {
		status = 409
		code = "conflict"
	}
	if errors.Is(e, ErrTaskNotReviewable) {
		status = http.StatusConflict
		code = "task_not_reviewable"
	}
	if errors.Is(e, ErrStaleRound) {
		status = http.StatusConflict
		code = "stale_review_round"
	}
	if errors.Is(e, ErrTooLate) {
		status = 409
		code = "too_late"
	}
	write(w, status, map[string]string{"code": code, "error": e.Error()})
}
