package ksef

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAuthenticateEncryptsTimestampAndRedeems(t *testing.T) {
	key, e := rsa.GenerateKey(rand.Reader, 2048)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now()
	der, e := x509.CreateCertificate(rand.Reader, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment}, &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fixture"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageKeyEncipherment}, &key.PublicKey, key)
	if e != nil {
		t.Fatal(e)
	}
	stamp := int64(1740000000123)
	steps := []string{"POST /auth/challenge", "GET /security/public-key-certificates", "POST /auth/ksef-token", "GET /auth/ref-1", "POST /auth/token/redeem"}
	i := 0
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if i >= len(steps) || r.Method+" "+r.URL.Path != steps[i] {
			t.Errorf("step %d got %s %s", i, r.Method, r.URL.Path)
			http.Error(w, "bad fixture", 500)
			return
		}
		switch i {
		case 0:
			json.NewEncoder(w).Encode(map[string]any{"challenge": "challenge-1", "timestampMs": stamp})
		case 1:
			json.NewEncoder(w).Encode([]any{map[string]any{"certificate": base64.StdEncoding.EncodeToString(der), "publicKeyId": "key-id", "validFrom": now.Add(-time.Hour), "validTo": now.Add(time.Hour), "usage": []string{"KsefTokenEncryption"}}})
		case 2:
			var req struct {
				Challenge   string            `json:"challenge"`
				Context     map[string]string `json:"contextIdentifier"`
				Encrypted   string            `json:"encryptedToken"`
				PublicKeyID string            `json:"publicKeyId"`
			}
			if e := json.NewDecoder(r.Body).Decode(&req); e != nil {
				t.Error(e)
			}
			cipher, _ := base64.StdEncoding.DecodeString(req.Encrypted)
			plain, e := rsa.DecryptOAEP(sha256.New(), rand.Reader, key, cipher, nil)
			if e != nil || string(plain) != "secret|1740000000123" || req.Challenge != "challenge-1" || req.Context["value"] != "5260250995" || req.PublicKeyID != "key-id" {
				t.Errorf("invalid encrypted auth request: %q %v %#v", plain, e, req)
			}
			w.WriteHeader(202)
			json.NewEncoder(w).Encode(map[string]any{"referenceNumber": "ref-1", "authenticationToken": map[string]any{"token": "auth-token", "validUntil": now.Add(time.Hour)}})
		case 3:
			if r.Header.Get("Authorization") != "Bearer auth-token" {
				t.Error("operation token not supplied")
			}
			json.NewEncoder(w).Encode(map[string]any{"status": map[string]any{"code": 200, "description": "ok"}})
		case 4:
			if r.Header.Get("Authorization") != "Bearer auth-token" {
				t.Error("redeem token not supplied")
			}
			json.NewEncoder(w).Encode(map[string]any{"accessToken": map[string]any{"token": "access", "validUntil": now.Add(time.Hour)}, "refreshToken": map[string]any{"token": "refresh", "validUntil": now.Add(time.Hour)}})
		}
		i++
	}))
	defer s.Close()
	c := New(s.URL, "5260250995")
	if e = c.Authenticate(context.Background(), "secret"); e != nil {
		t.Fatal(e)
	}
	if i != len(steps) {
		t.Fatalf("only %d requests", i)
	}
}
func TestQueryContractAndNoRedirect(t *testing.T) {
	called := false
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != "POST" || r.URL.Query().Get("pageOffset") != "2" || r.URL.Query().Get("pageSize") != "250" || r.URL.Query().Get("sortOrder") != "Asc" {
			t.Errorf("query URL: %s", r.URL)
		}
		var body map[string]any
		if e := json.NewDecoder(r.Body).Decode(&body); e != nil {
			t.Error(e)
		}
		dr := body["dateRange"].(map[string]any)
		if dr["dateType"] != "PermanentStorage" || dr["restrictToPermanentStorageHwmDate"] != true {
			t.Errorf("date range %#v", dr)
		}
		if body["subjectType"] != "Subject2" {
			t.Errorf("subject %v", body["subjectType"])
		}
		json.NewEncoder(w).Encode(map[string]any{"hasMore": false, "isTruncated": false, "permanentStorageHwmDate": "2025-01-01T00:00:00Z", "invoices": []any{}})
	}))
	defer s.Close()
	c := New(s.URL, "5260250995")
	c.access = "access"
	_, e := c.Query(context.Background(), "received", time.Now().Add(-time.Hour), time.Now(), 2)
	if e != nil {
		t.Fatal(e)
	}
	if !called {
		t.Fatal("request not made")
	}
	r := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "http://127.0.0.1:1/steal", 302) }))
	defer r.Close()
	c = New(r.URL, "5260250995")
	if _, _, e = c.request(context.Background(), "GET", "/x", nil, "secret", 0); e == nil || !strings.Contains(e.Error(), "HTTP 302") {
		t.Fatalf("redirect not rejected: %v", e)
	}
}
