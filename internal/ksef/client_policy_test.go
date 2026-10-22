package ksef

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetryAfter(t *testing.T) {
	now := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, tt := range []struct {
		value string
		want  time.Duration
	}{
		{"120", 120 * time.Second}, {now.Add(2 * time.Minute).Format(http.TimeFormat), 2 * time.Minute}, {now.Add(-time.Minute).Format(http.TimeFormat), 0}, {"invalid", 0}, {"-1", 0},
	} {
		if got := retryAfter(tt.value, now); got != tt.want {
			t.Errorf("%q: %s want %s", tt.value, got, tt.want)
		}
	}
	if retryAfter("18446744073709551615", now) <= 0 {
		t.Fatal("large Retry-After overflowed")
	}
}
func TestBoundedRetriesAndRedaction(t *testing.T) {
	for _, status := range []int{429, 500, 403} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Retry-After", "120")
				w.WriteHeader(status)
				w.Write([]byte(`{"code":"secret-token","detail":"private taxpayer XML"}`))
			}))
			defer server.Close()
			c := New(server.URL, "5260250995")
			var waits []time.Duration
			c.wait = func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return ctx.Err() }
			_, _, e := c.request(context.Background(), "GET", "/x", nil, "secret-token", 3)
			if e == nil || strings.Contains(e.Error(), "secret-token") || strings.Contains(e.Error(), "private taxpayer") {
				t.Fatal("unsafe error", e)
			}
			want := int32(4)
			if status == 403 {
				want = 1
				if !strings.Contains(e.Error(), "InvoiceRead") {
					t.Fatal("permission error not actionable", e)
				}
			}
			if calls.Load() != want || len(waits) != int(want)-1 {
				t.Fatal("unbounded retries", calls.Load(), waits)
			}
			if status != 403 {
				for _, wait := range waits {
					if wait < 120*time.Second {
						t.Fatal("Retry-After shortened", wait)
					}
				}
			}
		})
	}
}
func TestTransientRecoveryAndCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			return
		}
		w.Write([]byte("ok"))
	}))
	defer server.Close()
	c := New(server.URL, "5260250995")
	c.wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	b, _, e := c.request(context.Background(), "GET", "/x", nil, "", 3)
	if e != nil || string(b) != "ok" || calls.Load() != 2 {
		t.Fatal(string(b), e, calls.Load())
	}
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(429)
	}))
	defer blocked.Close()
	c = New(blocked.URL, "5260250995")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, _, e = c.request(ctx, "GET", "/x", nil, "", 3); !errors.Is(e, context.DeadlineExceeded) {
		t.Fatal("retry deadline lost", e)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, _, e = c.request(ctx, "GET", "/x", nil, "", 3); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}
func TestRefreshLifecycleAndControlledReplay(t *testing.T) {
	for _, scenario := range []string{"proactive", "401", "repeated401", "403", "refresh rejected", "expired refresh", "proactive rejected"} {
		t.Run(scenario, func(t *testing.T) {
			var requests, refreshes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/auth/token/refresh" {
					refreshes.Add(1)
					if r.Header.Get("Authorization") != "Bearer refresh-secret" {
						t.Error("wrong refresh token")
					}
					if scenario == "refresh rejected" {
						w.WriteHeader(403)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"accessToken": map[string]any{"token": "new-access", "validUntil": time.Now().Add(time.Hour)}})
					return
				}
				count := requests.Add(1)
				if scenario == "403" {
					w.WriteHeader(403)
					return
				}
				if scenario == "repeated401" || scenario == "proactive rejected" || ((scenario == "401" || scenario == "refresh rejected") && count == 1) {
					w.WriteHeader(401)
					return
				}
				if r.Header.Get("Authorization") != "Bearer new-access" {
					t.Error("wrong access token placement")
				}
				w.Write([]byte("ok"))
			}))
			defer server.Close()
			c := New(server.URL, "5260250995")
			c.access = "old-access"
			c.refresh = "refresh-secret"
			c.accessExpiry = time.Now().Add(time.Hour)
			c.refreshExpiry = time.Now().Add(2 * time.Hour)
			if strings.HasPrefix(scenario, "proactive") || scenario == "expired refresh" {
				c.accessExpiry = time.Now().Add(time.Second)
			}
			if scenario == "expired refresh" {
				c.refreshExpiry = time.Now().Add(-time.Second)
			}
			_, _, e := c.do(context.Background(), "GET", "/invoice", nil)
			wantError := !slices.Contains([]string{"proactive", "401"}, scenario)
			if (e != nil) != wantError {
				t.Fatal("unexpected refresh outcome", e)
			}
			if refreshes.Load() > 1 || requests.Load() > 2 {
				t.Fatal("unbounded refresh/replay", refreshes.Load(), requests.Load())
			}
			if scenario == "403" && refreshes.Load() != 0 {
				t.Fatal("403 refreshed")
			}
			if scenario == "expired refresh" && (refreshes.Load() != 0 || requests.Load() != 0) {
				t.Fatal("expired refresh sent")
			}
			if !wantError && (!c.accessExpiry.After(time.Now().Add(30*time.Minute)) || refreshes.Load() != 1) {
				t.Fatal("expiry not retained/refreshed")
			}
		})
	}
}
func encryptionCert(t *testing.T, public any, private any, from, to time.Time) cert {
	t.Helper()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: from, NotAfter: to, KeyUsage: x509.KeyUsageKeyEncipherment}
	der, e := x509.CreateCertificate(rand.Reader, template, template, public, private)
	if e != nil {
		t.Fatal(e)
	}
	return cert{Certificate: base64.StdEncoding.EncodeToString(der), PublicKeyID: "valid", ValidFrom: from, ValidTo: to, Usage: []string{"KsefTokenEncryption"}}
}
func TestAuthenticationFailureMatrix(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	valid := encryptionCert(t, &key.PublicKey, key, now.Add(-time.Hour), now.Add(time.Hour))
	ec, e := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	notRSA := encryptionCert(t, &ec.PublicKey, ec, now.Add(-time.Hour), now.Add(2*time.Hour))
	for _, scenario := range []string{"pending success", "terminal failure", "cancel polling", "redeem failure", "start failure", "wrong usage", "expired", "future", "expired DER", "non RSA", "select valid RSA"} {
		t.Run(scenario, func(t *testing.T) {
			var starts, polls, redeems atomic.Int32
			certs := []cert{valid}
			switch scenario {
			case "wrong usage":
				certs[0].Usage = []string{"SymmetricKeyEncryption"}
			case "expired":
				certs[0].ValidTo = now.Add(-time.Second)
			case "future":
				certs[0].ValidFrom = now.Add(time.Hour)
			case "expired DER":
				certs[0] = encryptionCert(t, &key.PublicKey, key, now.Add(-2*time.Hour), now.Add(-time.Hour))
				certs[0].ValidTo = now.Add(time.Hour)
			case "non RSA":
				certs = []cert{notRSA}
			case "select valid RSA":
				certs = []cert{notRSA, valid}
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/auth/challenge":
					json.NewEncoder(w).Encode(challenge{Challenge: "fixture", TimestampMs: 1740000000123})
				case "/security/public-key-certificates":
					json.NewEncoder(w).Encode(certs)
				case "/auth/ksef-token":
					starts.Add(1)
					if scenario == "start failure" {
						w.WriteHeader(500)
						return
					}
					var payload map[string]any
					json.NewDecoder(r.Body).Decode(&payload)
					if payload["publicKeyId"] != "valid" {
						t.Error("wrong encryption key selected")
					}
					json.NewEncoder(w).Encode(map[string]any{"referenceNumber": "ref", "authenticationToken": map[string]string{"token": "auth"}})
				case "/auth/ref":
					count := polls.Add(1)
					code := 200
					if scenario == "terminal failure" {
						code = 450
					}
					if scenario == "cancel polling" || (scenario == "pending success" && count == 1) {
						code = 100
					}
					json.NewEncoder(w).Encode(map[string]any{"status": map[string]int{"code": code}})
				case "/auth/token/redeem":
					redeems.Add(1)
					if scenario == "redeem failure" {
						w.WriteHeader(500)
						return
					}
					json.NewEncoder(w).Encode(map[string]any{"accessToken": map[string]any{"token": "access", "validUntil": now.Add(time.Hour)}, "refreshToken": map[string]any{"token": "refresh", "validUntil": now.Add(2 * time.Hour)}})
				default:
					t.Error("unexpected route", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			c := New(server.URL, "5260250995")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c.wait = func(ctx context.Context, _ time.Duration) error {
				if scenario == "cancel polling" {
					cancel()
				}
				return ctx.Err()
			}
			e := c.Authenticate(ctx, "secret")
			success := scenario == "pending success" || scenario == "select valid RSA"
			if (e == nil) != success {
				t.Fatal("unexpected auth result", e)
			}
			if starts.Load() > 1 || redeems.Load() > 1 {
				t.Fatal("ambiguous auth POST retried", starts.Load(), redeems.Load())
			}
			if scenario == "cancel polling" && !errors.Is(e, context.Canceled) {
				t.Fatal(e)
			}
			if !success && scenario != "redeem failure" && redeems.Load() != 0 {
				t.Fatal("failed authentication redeemed")
			}
			if slices.Contains([]string{"wrong usage", "expired", "future", "expired DER", "non RSA"}, scenario) && starts.Load() != 0 {
				t.Fatal("invalid certificate used")
			}
		})
	}
}
