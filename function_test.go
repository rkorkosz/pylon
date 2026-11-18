package pylon

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestServe(t *testing.T) {
	for _, tc := range []struct {
		method, path     string
		code             int
		invokes, preview bool
	}{
		{"GET", "/", 405, false, false},
		{"POST", "/other", 404, false, false},
		{"POST", "/", 503, true, false},
		{"POST", "/preview", 503, true, true},
	} {
		req := httptest.NewRequestWithContext(t.Context(), tc.method, tc.path, nil)
		rec := httptest.NewRecorder()
		called := false
		serve(rec, req, func(ctx context.Context, preview bool) int {
			called = true
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > workTimeout || preview != tc.preview {
				t.Fatal(deadline, preview)
			}
			return http.StatusServiceUnavailable
		})
		if rec.Code != tc.code || called != tc.invokes || rec.Body.Len() != 0 {
			t.Fatal(rec.Code, called, rec.Body.String())
		}
		if rec.Code == 405 && rec.Header().Get("Allow") != "POST" {
			t.Fatal(rec.Header())
		}
	}
}

func TestLoadSettings(t *testing.T) {
	values := map[string]string{"PYLON_FIRESTORE_PROJECT": "demo-pylon", "PYLON_FIRESTORE_DATABASE": "(default)", "PYLON_DEPLOYMENT": "pylon-test", "PYLON_TASK_LIST": "dedicated_list", "PYLON_ENVIRONMENT": "test", "PYLON_NIP": "5260250995"}
	lookup := func(k string) string { return values[k] }
	if s, err := loadSettings(lookup); err != nil || s.ksef.Subject != "received" || s.warsaw.String() != "Europe/Warsaw" {
		t.Fatal(s, err)
	}
	for _, key := range []string{"PYLON_ENVIRONMENT", "PYLON_TASK_LIST", "PYLON_NIP", "PYLON_FIRESTORE_PROJECT", "PYLON_FIRESTORE_DATABASE", "PYLON_DEPLOYMENT"} {
		v := values[key]
		delete(values, key)
		if _, err := loadSettings(lookup); err == nil {
			t.Fatal("accepted missing", key)
		}
		values[key] = v
	}
	values["PYLON_TASK_LIST"] = "@default"
	if _, err := loadSettings(lookup); err == nil {
		t.Fatal("accepted default list alias")
	}
}
