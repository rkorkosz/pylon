// Package pylon exposes the private Cloud Run function for invoice reminders.
package pylon

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"
	_ "time/tzdata"

	"cloud.google.com/go/firestore"
	"github.com/GoogleCloudPlatform/functions-framework-go/functions"
	"github.com/rkrokosz/pylon/internal/cloudstate"
	"github.com/rkrokosz/pylon/internal/config"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/runner"
	"github.com/rkrokosz/pylon/internal/storage"
	"github.com/rkrokosz/pylon/internal/sync"
	"github.com/rkrokosz/pylon/internal/tasks"
)

const (
	workTimeout      = 12 * time.Minute
	discoveryTimeout = 6 * time.Minute
	leaseTimeout     = 15 * time.Minute
	ksefSecret       = "/var/secrets/pylon-ksef/token"
	googleSecret     = "/var/secrets/pylon-google/oauth.json"
)

var logger = slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
	if a.Key == slog.LevelKey {
		a.Key = "severity"
	}
	if a.Key == slog.MessageKey {
		a.Key = "message"
	}
	return a
}}))

func init() { functions.HTTP("SyncInvoices", SyncInvoices) }

type settings struct {
	ksef                                config.Config
	project, database, deployment, list string
	warsaw                              *time.Location
}

var (
	label     = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)
	projectID = regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	listID    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)
)

func loadSettings(env func(string) string) (settings, error) {
	s := settings{project: env("PYLON_FIRESTORE_PROJECT"), database: env("PYLON_FIRESTORE_DATABASE"), deployment: env("PYLON_DEPLOYMENT"), list: env("PYLON_TASK_LIST")}
	databaseValid := s.database == "(default)" || label.MatchString(s.database)
	if !projectID.MatchString(s.project) || !databaseValid || !label.MatchString(s.deployment) || !listID.MatchString(s.list) {
		return settings{}, errors.New("invalid cloud configuration")
	}
	var err error
	s.ksef, err = config.ValidateScope(env("PYLON_ENVIRONMENT"), env("PYLON_NIP"))
	if err != nil {
		return settings{}, err
	}
	s.ksef.Subject = "received"
	s.warsaw, err = time.LoadLocation("Europe/Warsaw")
	return s, err
}

// SyncInvoices relies on Cloud Run IAM for authentication. No caller-provided
// dates, credentials or destination overrides are accepted.
func SyncInvoices(w http.ResponseWriter, req *http.Request) {
	serve(w, req, execute)
}

func serve(w http.ResponseWriter, req *http.Request, run func(context.Context, bool) int) {
	if req.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if req.URL.Path != "/" && req.URL.Path != "/preview" {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	ctx, cancel := context.WithTimeout(req.Context(), workTimeout)
	defer cancel()
	w.WriteHeader(run(ctx, req.URL.Path == "/preview"))
}

func execute(ctx context.Context, preview bool) int {
	now := time.Now().UTC()
	s, err := loadSettings(os.Getenv)
	if err != nil || (os.Getenv("K_SERVICE") != "" && os.Getenv("FIRESTORE_EMULATOR_HOST") != "") {
		return failure("configuration", http.StatusInternalServerError)
	}
	source := func(ctx context.Context) (sync.Downloader, error) {
		b, err := os.ReadFile(ksefSecret)
		if err != nil || strings.TrimSpace(string(b)) == "" {
			return nil, ksef.ErrAuthorization
		}
		client := ksef.New(s.ksef.BaseURL, s.ksef.Nip)
		if err := client.Authenticate(ctx, strings.TrimSpace(string(b))); err != nil {
			return nil, err
		}
		return client, nil
	}
	start := cloudstate.MonthStart(now, s.warsaw)
	if preview {
		return previewBootstrap(ctx, source, start, now, s.warsaw)
	}

	b, err := os.ReadFile(googleSecret)
	var oauth tasks.Credentials
	if err != nil || json.Unmarshal(b, &oauth) != nil || strings.TrimSpace(oauth.ClientID) == "" || strings.TrimSpace(oauth.RefreshToken) == "" || strings.TrimSpace(oauth.ClientSecret) == "" {
		return failure("google_secret", http.StatusInternalServerError)
	}
	db, err := firestore.NewClientWithDatabase(ctx, s.project, s.database)
	if err != nil {
		return stateFailure(err)
	}
	defer func() { _ = db.Close() }()
	state, err := cloudstate.Acquire(ctx, db, s.deployment, s.ksef.Environment, s.ksef.Nip, s.list, start, now.Add(leaseTimeout))
	if err != nil {
		return stateFailure(err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		if err := state.Release(cleanup); err != nil {
			logger.Warn("lease_release_failed")
		}
	}()
	out := runner.Run(ctx, state, source, tasks.New(ctx, s.list, oauth), now, discoveryTimeout, s.warsaw, time.Now)
	complete := out.Complete() && ctx.Err() == nil
	level := slog.LevelInfo
	if !complete {
		level = slog.LevelWarn
	}
	if out.Unresolved > 0 || out.AuthorizationError {
		level = slog.LevelError
	}
	logger.Log(ctx, level, "invoice_sync", "complete", complete, "counts", out)
	if complete {
		return http.StatusNoContent
	}
	if out.AuthorizationError {
		return http.StatusInternalServerError
	}
	return http.StatusServiceUnavailable
}

func stateFailure(err error) int {
	if errors.Is(err, cloudstate.ErrLease) {
		return failure("lease_busy", http.StatusServiceUnavailable)
	}
	return failure("state_unavailable", http.StatusServiceUnavailable)
}

func failure(reason string, code int) int {
	logger.Error("invoice_sync_failed", "reason", reason)
	return code
}

// Preview simulates the first month's discovery with no writes to Firestore or Tasks.
func previewBootstrap(ctx context.Context, source runner.Source, start, end time.Time, warsaw *time.Location) int {
	api, err := source(ctx)
	if err != nil {
		return failure("preview_source_unavailable", http.StatusServiceUnavailable)
	}
	preview := previewState{cursor: start, today: end.In(warsaw), seen: make(map[string]bool)}
	out, err := sync.Received(ctx, api, &preview, end)
	complete := err == nil && ctx.Err() == nil
	logger.Info("invoice_preview", "complete", complete, "unique_invoices", out.Downloaded, "would_create", preview.eligible, "would_skip", out.Downloaded-preview.eligible, "coverage_start", start, "covered_through", out.CompletedThrough)
	if !complete {
		return http.StatusServiceUnavailable
	}
	return http.StatusNoContent
}

type previewState struct {
	cursor, today time.Time
	seen          map[string]bool
	eligible      int
}

func (p *previewState) Cursor(context.Context, string) (time.Time, bool, error) {
	return p.cursor, true, nil
}
func (p *previewState) SaveCursor(_ context.Context, _ string, t time.Time) error {
	p.cursor = t
	return nil
}
func (p *previewState) Existing(_ context.Context, n string, _ storage.Metadata) (bool, error) {
	return p.seen[n], nil
}
func (p *previewState) Save(ctx context.Context, n string, b []byte, m storage.Metadata) (bool, error) {
	i, err := cloudstate.Prepare(ctx, n, b, m)
	if err != nil {
		return false, err
	}
	var due string
	if i.Status == cloudstate.Pending {
		due, err = runner.DueDate(i.Deadline, p.today)
		if err != nil {
			return false, err
		}
	}
	if due != "" {
		p.eligible++
	}
	p.seen[n] = true
	return due == "", nil
}
