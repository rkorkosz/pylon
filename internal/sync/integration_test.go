package sync_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
	syncer "github.com/rkrokosz/pylon/internal/sync"
)

func TestAuthenticatedSyncInterruptedRestartAndNoop(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment}
	der, e := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	start := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	var failB atomic.Bool
	failB.Store(true)
	var queryCount atomic.Int32
	var authCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/auth/challenge":
			if err := json.NewEncoder(w).Encode(map[string]any{"challenge": "c", "timestampMs": int64(1740000000123)}); err != nil {
				t.Error(err)
			}
		case "/security/public-key-certificates":
			if err := json.NewEncoder(w).Encode([]any{map[string]any{"certificate": base64.StdEncoding.EncodeToString(der), "publicKeyId": "kid", "validFrom": now.Add(-time.Hour), "validTo": now.Add(time.Hour), "usage": []string{"KsefTokenEncryption"}}}); err != nil {
				t.Error(err)
			}
		case "/auth/ksef-token":
			w.WriteHeader(202)
			if err := json.NewEncoder(w).Encode(map[string]any{"referenceNumber": "ref", "authenticationToken": map[string]any{"token": "operation", "validUntil": now.Add(time.Hour)}}); err != nil {
				t.Error(err)
			}
		case "/auth/ref":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"code": 200, "description": "ok"}})
		case "/auth/token/redeem":
			authCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"accessToken": map[string]any{"token": "access", "validUntil": now.Add(time.Hour)}, "refreshToken": map[string]any{"token": "refresh", "validUntil": now.Add(time.Hour)}})
		case "/invoices/query/metadata":
			queryCount.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"hasMore": false, "isTruncated": false, "permanentStorageHwmDate": end, "invoices": []any{map[string]any{"ksefNumber": "5260250995-20250101-000000000001-00", "permanentStorageDate": start.Add(time.Hour)}, map[string]any{"ksefNumber": "5260250995-20250101-000000000002-00", "permanentStorageDate": start.Add(2 * time.Hour)}}})
		case "/invoices/ksef/5260250995-20250101-000000000001-00":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<Invoice>A</Invoice>`))
		case "/invoices/ksef/5260250995-20250101-000000000002-00":
			if failB.CompareAndSwap(true, false) {
				http.Error(w, "denied", http.StatusForbidden)
				return
			}
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<Invoice>B</Invoice>`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store, e := storage.Open(filepath.Join(t.TempDir(), "data"), "test", "5260250995")
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	to := end
	run := func(from *time.Time) ([]syncer.Summary, error) {
		api := ksef.New(server.URL, "5260250995")
		if e := api.Authenticate(context.Background(), "synthetic"); e != nil {
			return nil, e
		}
		return syncer.Run(context.Background(), api, store, "test", "5260250995", []string{"received"}, from, &to)
	}
	if _, e = run(&start); e == nil {
		t.Fatal("expected injected document failure")
	}
	if _, ok, e := store.Checkpoint("received"); e != nil || ok {
		t.Fatalf("interrupted window checkpointed: %v %v", ok, e)
	}
	if ok, e := store.ExistingValid("5260250995-20250101-000000000001-00"); e != nil || !ok {
		t.Fatalf("first durable document missing: %v %v", ok, e)
	}
	if _, e = run(&start); e != nil {
		t.Fatal(e)
	}
	if ok, e := store.ExistingValid("5260250995-20250101-000000000002-00"); e != nil || !ok {
		t.Fatalf("resumed document missing: %v %v", ok, e)
	}
	cp, ok, e := store.Checkpoint("received")
	if e != nil || !ok || !cp.Cursor.Equal(end) {
		t.Fatalf("checkpoint %#v %v %v", cp, ok, e)
	}
	before := queryCount.Load()
	if _, e = run(nil); e != nil {
		t.Fatal(e)
	}
	if queryCount.Load() != before {
		t.Fatal("completed repeat issued metadata query")
	}
	if authCount.Load() != 3 {
		t.Fatalf("auth cycles=%d", authCount.Load())
	}
}
