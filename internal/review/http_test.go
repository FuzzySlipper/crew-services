package review

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRetryFailedHTTPContract(t *testing.T) {
	svc, store, _, _, admission := fixture(t, 1)
	defer store.Close()
	job, _, err := svc.Admit(context.Background(), admission)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.Fail(context.Background(), job.ID, Failed, "reviewer failed"); err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(svc)
	request := httptest.NewRequest(http.MethodPost, "/v1/review-jobs/"+job.ID+"/retry", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("retry status=%d body=%s", response.Code, response.Body.String())
	}
	var body struct {
		Job     Projection `json:"job"`
		Retried bool       `json:"retried"`
	}
	if err := json.NewDecoder(response.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if !body.Retried || body.Job.ID != job.ID || body.Job.State != Queued || body.Job.Failure != "" {
		t.Fatalf("retry response=%+v", body)
	}

	for name, testCase := range map[string]struct {
		path       string
		wantStatus int
		wantCode   string
	}{
		"duplicate": {path: "/v1/review-jobs/" + job.ID + "/retry", wantStatus: http.StatusConflict, wantCode: "too_late"},
		"missing":   {path: "/v1/review-jobs/missing/retry", wantStatus: http.StatusNotFound, wantCode: "not_found"},
	} {
		t.Run(name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, testCase.path, nil))
			if response.Code != testCase.wantStatus {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			var errorBody map[string]string
			if err := json.NewDecoder(response.Body).Decode(&errorBody); err != nil {
				t.Fatal(err)
			}
			if errorBody["code"] != testCase.wantCode {
				t.Fatalf("error response=%v", errorBody)
			}
		})
	}
}
