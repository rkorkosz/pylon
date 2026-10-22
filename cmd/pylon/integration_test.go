package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/rkrokosz/pylon/internal/config"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
)

// Only the test binary accepts a fixture URL. The shipped CLI has no such flag
// or environment override, and still sends credentials only to allowed hosts.
func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("PYLON_TEST_HELPER") != "1" {
		return
	}
	separator := 0
	for i, a := range os.Args {
		if a == "--" {
			separator = i + 1
			break
		}
	}
	if separator == 0 {
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := runCLI(ctx, os.Args[separator:], os.Stdout, func(c config.Config) *ksef.Client { return ksef.New(os.Getenv("PYLON_TEST_URL"), c.Nip) })
	if err != nil {
		fmt.Fprintln(os.Stderr, "pylon:", err)
		os.Exit(1)
	}
	os.Exit(0)
}
func TestCLIProcessCrashRestartAndNoop(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	const nip = "5260250995"
	const a = "5260250995-20250101-000000000001-00"
	const b = "5260250995-20250101-000000000002-00"
	documents := map[string][]byte{a: []byte("<?xml version=\"1.0\"?>\n<Invoice>synthetic-private-A</Invoice>\n"), b: []byte("<Invoice>synthetic-private-B</Invoice>\n")}
	start := time.Date(2025, 1, 1, 0, 0, 0, 123456789, time.UTC)
	end := start.Add(24 * time.Hour)
	var interrupt atomic.Bool
	interrupt.Store(true)
	var queries, auths atomic.Int32
	secondPage := make(chan struct{}, 1)
	releaseInterrupted := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expectedToken := ""
		switch r.URL.Path {
		case "/auth/ref", "/auth/token/redeem":
			expectedToken = "Bearer synthetic-operation-secret"
		case "/invoices/query/metadata":
			expectedToken = "Bearer synthetic-access-secret"
		default:
			if strings.HasPrefix(r.URL.Path, "/invoices/ksef/") {
				expectedToken = "Bearer synthetic-access-secret"
			}
		}
		if r.Header.Get("Authorization") != expectedToken {
			t.Errorf("wrong token for %s", r.URL.Path)
			w.WriteHeader(403)
			return
		}
		switch r.URL.Path {
		case "/auth/challenge":
			_ = json.NewEncoder(w).Encode(map[string]any{"challenge": "fixture", "timestampMs": 1740000000123})
		case "/security/public-key-certificates":
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"certificate": base64.StdEncoding.EncodeToString(der), "publicKeyId": "fixture", "validFrom": now.Add(-time.Hour), "validTo": now.Add(time.Hour), "usage": []string{"KsefTokenEncryption"}}})
		case "/auth/ksef-token":
			_ = json.NewEncoder(w).Encode(map[string]any{"referenceNumber": "ref", "authenticationToken": map[string]string{"token": "synthetic-operation-secret"}})
		case "/auth/ref":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]int{"code": 200}})
		case "/auth/token/redeem":
			auths.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": map[string]any{"token": "synthetic-access-secret", "validUntil": now.Add(time.Hour)}, "refreshToken": map[string]any{"token": "synthetic-refresh-secret", "validUntil": now.Add(2 * time.Hour)}})
		case "/invoices/query/metadata":
			queries.Add(1)
			offset := r.URL.Query().Get("pageOffset")
			if offset == "1" && interrupt.Load() {
				secondPage <- struct{}{}
				select {
				case <-r.Context().Done():
				case <-releaseInterrupted:
				}
				return
			}
			name := a
			more := true
			if offset == "1" {
				name = b
				more = false
			} else if offset != "0" {
				t.Error("unexpected page", offset)
			}
			var body struct {
				SubjectType string `json:"subjectType"`
				DateRange   struct {
					From, To time.Time
					DateType string
					Restrict bool `json:"restrictToPermanentStorageHwmDate"`
				} `json:"dateRange"`
			}
			if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
				t.Error(e)
			}
			if body.SubjectType != "Subject2" || body.DateRange.DateType != "PermanentStorage" || !body.DateRange.Restrict || !body.DateRange.From.Equal(start) || !body.DateRange.To.Equal(end) {
				t.Errorf("invalid query: %#v", body)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"hasMore": more, "isTruncated": false, "permanentStorageHwmDate": end, "invoices": []any{map[string]any{"ksefNumber": name, "permanentStorageDate": start.Add(time.Hour)}}})
		default:
			name := strings.TrimPrefix(r.URL.Path, "/invoices/ksef/")
			if data, ok := documents[name]; ok {
				w.Header().Set("Content-Type", "application/xml")
				_, _ = w.Write(data)
			} else {
				http.NotFound(w, r)
			}
		}
	}))
	defer func() { close(releaseInterrupted); server.Close() }()
	root := filepath.Join(t.TempDir(), "data")
	args := []string{"sync", "--nip", nip, "--from", start.Format(time.RFC3339Nano), "--to", end.Format(time.RFC3339Nano), "--storage-dir", root}
	executable, e := os.Executable()
	if e != nil {
		t.Fatal(e)
	}
	newCommand := func(withFrom bool) (*exec.Cmd, context.CancelFunc) {
		argv := args
		if !withFrom {
			argv = append(append([]string{}, args[:3]...), args[5:]...)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		cmd := exec.CommandContext(ctx, executable, append([]string{"-test.run=^TestCLIHelperProcess$", "--"}, argv...)...)
		cmd.Env = append(os.Environ(), "PYLON_TEST_HELPER=1", "PYLON_TEST_URL="+server.URL, "PYLON_KSEF_TOKEN=synthetic-ksef-secret")
		return cmd, cancel
	}
	var logs bytes.Buffer
	cmd, cancel := newCommand(true)
	defer cancel()
	cmd.Stdout = &logs
	cmd.Stderr = &logs
	if e = cmd.Start(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-secondPage:
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill() // Best-effort cleanup before reporting the timeout.
		_ = cmd.Wait()         // A killed process is expected to fail.
		t.Fatal("interrupted run did not reach page 2", logs.String())
	}
	if e = cmd.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	if e = cmd.Wait(); e == nil {
		t.Fatal("killed CLI succeeded")
	}
	s, e := storage.Open(root, "test", nip)
	if e != nil {
		t.Fatal("process exit did not release lock", e)
	}
	if _, ok, e := s.Checkpoint("received"); e != nil || ok {
		t.Fatal("unfinished window checkpointed", ok, e)
	}
	if ok, e := s.ExistingValid(a); e != nil || !ok {
		t.Fatal("first document not durable", ok, e)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	interrupt.Store(false)
	cmd, cancel2 := newCommand(true)
	defer cancel2()
	output, e := cmd.CombinedOutput()
	logs.Write(output)
	if e != nil {
		t.Fatal("restart failed", e, string(output))
	}
	if !bytes.Contains(output, []byte("downloaded=1 already_present=1 failed=0")) {
		t.Fatal("incorrect restart summary", string(output))
	}
	s, e = storage.Open(root, "test", nip)
	if e != nil {
		t.Fatal(e)
	}
	cp, ok, e := s.Checkpoint("received")
	if e != nil || !ok || !cp.Cursor.Equal(end) {
		t.Fatal(cp, ok, e)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	before := queries.Load()
	cmd, cancel3 := newCommand(false)
	defer cancel3()
	output, e = cmd.CombinedOutput()
	logs.Write(output)
	if e != nil || queries.Load() != before {
		t.Fatal("completed repeat was not a no-op", e, string(output))
	}
	if auths.Load() != 3 {
		t.Fatal("authentication cycles", auths.Load())
	}
	files, e := filepath.Glob(filepath.Join(root, "test", nip, "invoices", "*.xml"))
	if e != nil || len(files) != 2 {
		t.Fatal("duplicate/missing XML", files, e)
	}
	for name, want := range documents {
		got, e := os.ReadFile(filepath.Join(root, "test", nip, "invoices", name+".xml"))
		if e != nil || !bytes.Equal(got, want) {
			t.Fatal("XML bytes changed", name, e)
		}
	}
	for _, secret := range []string{"synthetic-ksef-secret", "synthetic-operation-secret", "synthetic-access-secret", "synthetic-refresh-secret", "synthetic-private-A", "synthetic-private-B"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatal("sensitive data in CLI output")
		}
	}
}
