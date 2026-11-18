package tasks

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/oauth2"
)

type tokenError struct{}

func (tokenError) Token() (*oauth2.Token, error) {
	return nil, &oauth2.RetrieveError{ErrorCode: "invalid_grant", Response: &http.Response{StatusCode: 400}}
}

func TestInsertOutcomes(t *testing.T) {
	for _, tc := range []struct {
		name, body, id  string
		status          int
		uncertain, auth bool
	}{
		{"success", `{"id":"task-id"}`, "task-id", 200, false, false},
		{"no ID", `{}`, "", 200, true, false},
		{"permission denied", "", "", 403, false, true},
		{"server failure", "", "", 503, true, false},
		{"rate limited", "", "", 429, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := make(chan string, 4)
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				calls <- string(b)
				if r.Method != http.MethodPost || r.Header.Get("Authorization") != "Bearer test" {
					t.Error("missing POST or auth")
				}
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := Client{endpoint: srv.URL, http: srv.Client(), tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"})}
			id, unknown, err := c.Insert(t.Context(), Task{Due: "2026-03-29T00:00:00Z"})
			if id != tc.id || unknown != tc.uncertain || errors.Is(err, ErrAuthorization) != tc.auth || (err == nil) != (id != "") {
				t.Fatal(id, unknown, err)
			}
			if len(calls) != 1 || !strings.Contains(<-calls, `"due":"2026-03-29T00:00:00Z"`) {
				t.Fatal("unexpected insert attempts or due date")
			}
			c.tokens = tokenError{}
			if _, unknown, err := c.Insert(t.Context(), Task{}); unknown || !errors.Is(err, ErrAuthorization) || len(calls) != 0 {
				t.Fatal(unknown, err)
			}
		})
	}
}

func TestMarkedPagination(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if q.Get("showCompleted") != "true" || q.Get("showHidden") != "true" {
			t.Error(q)
		}
		if q.Get("pageToken") == "" {
			_, _ = io.WriteString(w, `{"nextPageToken":"next","items":[{"id":"deleted","status":"completed","deleted":true,"notes":"pylon:v1:mark"},{"id":"wrong","notes":"not pylon:v1:mark"}]}`)
			return
		}
		if q.Get("pageToken") != "next" {
			t.Error(q)
		}
		_, _ = io.WriteString(w, `{"items":[{"id":"completed","status":"completed","hidden":true,"notes":"Deadline\npylon:v1:mark"}]}`)
	}))
	defer srv.Close()
	c := Client{endpoint: srv.URL, http: srv.Client(), tokens: oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "test"})}
	matches, err := c.Marked(t.Context())
	got := matches[Marker("mark")]
	if err != nil || len(got) != 1 || got[0].ID != "completed" || !got[0].Paid() {
		t.Fatal(matches, err)
	}
	if (Task{Status: "completed", Deleted: true}).Paid() || (Task{Status: "needsAction"}).Paid() {
		t.Fatal("nonpayment treated as paid")
	}
}
