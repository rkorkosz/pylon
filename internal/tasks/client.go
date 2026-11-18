// Package tasks implements date-only Google Tasks delivery with explicit insert outcomes.
package tasks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

const Scope = "https://www.googleapis.com/auth/tasks"

var ErrAuthorization = errors.New("authorization or destination rejected by Google Tasks")

type Credentials struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

type Client struct {
	endpoint string
	http     *http.Client
	tokens   oauth2.TokenSource
}

func New(ctx context.Context, list string, credentials Credentials) *Client {
	h := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	cfg := oauth2.Config{ClientID: credentials.ClientID, ClientSecret: credentials.ClientSecret, Endpoint: google.Endpoint, Scopes: []string{Scope}}
	return &Client{
		endpoint: "https://tasks.googleapis.com/tasks/v1/lists/" + url.PathEscape(list) + "/tasks",
		http:     h,
		tokens:   cfg.TokenSource(context.WithValue(ctx, oauth2.HTTPClient, h), &oauth2.Token{RefreshToken: credentials.RefreshToken}),
	}
}

type Task struct {
	ID      string `json:"id,omitempty"`
	Title   string `json:"title,omitempty"`
	Notes   string `json:"notes,omitempty"`
	Due     string `json:"due,omitempty"`
	Status  string `json:"status,omitempty"`
	Deleted bool   `json:"deleted,omitempty"`
}

func (t Task) Paid() bool     { return !t.Deleted && t.Status == "completed" }
func Marker(id string) string { return "pylon:v1:" + id }

func (c *Client) request(ctx context.Context, method, endpoint string, body []byte) (*http.Response, bool, error) {
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	token, err := c.tokens.Token()
	if err != nil {
		var retrieve *oauth2.RetrieveError
		if errors.As(err, &retrieve) && retrieve.Response != nil && (retrieve.ErrorCode == "invalid_grant" || rejected(retrieve.Response.StatusCode)) {
			return nil, false, ErrAuthorization
		}
		return nil, false, errors.New("unable to obtain Google OAuth token")
	}
	token.SetAuthHeader(req)
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := c.http.Do(req)
	return res, true, err
}

func rejected(code int) bool {
	return code == 400 || code == 401 || code == 403 || code == 404
}

// Insert returns uncertain=true if a task may exist without a known ID. Such
// an outcome must never be retried as an insert. Only explicit throttling is
// retried here; transport errors and server errors are ambiguous.
func (c *Client) Insert(ctx context.Context, task Task) (id string, uncertain bool, err error) {
	body, err := json.Marshal(task)
	if err != nil {
		return "", false, err
	}
	for attempt := range 3 {
		res, sent, err := c.request(ctx, http.MethodPost, c.endpoint, body)
		if err != nil {
			return "", sent, err
		}
		var inserted Task
		if res.StatusCode >= 200 && res.StatusCode < 300 {
			err = decode(res, &inserted)
			if err != nil || inserted.ID == "" {
				return "", true, errors.New("task insert returned no usable ID")
			}
			return inserted.ID, false, nil
		}
		_ = res.Body.Close()
		if rejected(res.StatusCode) {
			return "", false, ErrAuthorization
		}
		if res.StatusCode != http.StatusTooManyRequests {
			return "", true, errors.New("task insert outcome is unknown")
		}
		if attempt == 2 {
			break
		}
		// Re-evaluate the Warsaw due date on the next invocation instead of
		// sleeping across a date change for very long Retry-After values.
		delay := retryDelay(res.Header.Get("Retry-After"), time.Now(), time.Second<<attempt)
		if delay > 10*time.Second {
			break
		}
		select {
		case <-ctx.Done():
			return "", false, ctx.Err()
		case <-time.After(delay):
			// The caller's date policy should not be bypassed at midnight.
			if before, ok := ctx.Value(dateCheckKey{}).(func() bool); ok && !before() {
				return "", false, errors.New("task date changed during retry")
			}
		}
	}
	return "", false, errors.New("rate limited by Google Tasks")
}

type dateCheckKey struct{}

// WithDateCheck stops in-run retries when the caller's calendar date changes.
func WithDateCheck(ctx context.Context, sameDate func() bool) context.Context {
	return context.WithValue(ctx, dateCheckKey{}, sameDate)
}

func retryDelay(header string, now time.Time, fallback time.Duration) time.Duration {
	if n, err := strconv.ParseUint(header, 10, 64); err == nil {
		if n > 10 {
			return 11 * time.Second
		}
		return max(fallback, time.Duration(n)*time.Second)
	}
	if t, err := http.ParseTime(header); err == nil {
		return max(fallback, t.Sub(now))
	}
	return fallback
}

func decode(res *http.Response, out any) error {
	defer func() { _ = res.Body.Close() }()
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(out)
}

// Marked uses all pages, including completed and hidden tasks. Only exact
// marker lines count. A deleted task is not payment evidence or a match.
func (c *Client) Marked(ctx context.Context) (map[string][]Task, error) {
	matches := make(map[string][]Task)
	seen := make(map[string]bool)
	q := url.Values{"showCompleted": {"true"}, "showHidden": {"true"}, "maxResults": {"100"}}
	for {
		res, _, err := c.request(ctx, http.MethodGet, c.endpoint+"?"+q.Encode(), nil)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			_ = res.Body.Close()
			if rejected(res.StatusCode) {
				return nil, ErrAuthorization
			}
			return nil, errors.New("task list unavailable")
		}
		var page struct {
			Items         []Task
			NextPageToken string
		}
		if err := decode(res, &page); err != nil {
			return nil, err
		}
		for _, task := range page.Items {
			if task.Deleted || task.ID == "" {
				continue
			}
			for line := range strings.SplitSeq(task.Notes, "\n") {
				line = strings.TrimSuffix(line, "\r")
				if strings.HasPrefix(line, "pylon:v1:") {
					matches[line] = append(matches[line], Task{ID: task.ID, Status: task.Status})
					break
				}
			}
		}
		next := page.NextPageToken
		if next == "" {
			return matches, nil
		}
		if seen[next] {
			return nil, errors.New("repeated Google Tasks page token")
		}
		seen[next] = true
		q.Set("pageToken", next)
	}
}
