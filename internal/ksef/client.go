package ksef

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const maxResponse = 64 << 20

type Client struct {
	base, nip                   string
	http                        *http.Client
	access, refresh             string
	accessExpiry, refreshExpiry time.Time
	wait                        func(context.Context, time.Duration) error
}

func New(base, nip string) *Client {
	return &Client{base: strings.TrimRight(base, "/"), nip: nip, wait: sleep, http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (c *Client) SetHTTPClient(h *http.Client) {
	if h != nil {
		c.http = h
	}
}

type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("KSeF API returned HTTP %d: %s", e.Status, e.Message)
}
func (c *Client) request(ctx context.Context, method, path string, body any, token string, retries int) ([]byte, http.Header, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	var data []byte
	var err error
	if body != nil {
		data, err = json.Marshal(body)
		if err != nil {
			return nil, nil, err
		}
	}
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, method, c.base+path, bytes.NewReader(data))
		if err != nil {
			return nil, nil, err
		}
		req.Header.Set("Accept", "application/json, application/xml")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			if attempt < retries && ctx.Err() == nil {
				if err = c.wait(ctx, backoff(attempt)); err == nil {
					continue
				}
			}
			return nil, nil, err
		}
		b, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		_ = resp.Body.Close() // The response has been read; retain any read error.
		if readErr != nil {
			return nil, nil, readErr
		}
		if len(b) > maxResponse {
			return nil, nil, errors.New("KSeF response exceeded size limit")
		}
		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			return b, resp.Header, nil
		}
		if (resp.StatusCode == 429 || resp.StatusCode >= 500) && attempt < retries {
			wait := backoff(attempt)
			if delay := retryAfter(resp.Header.Get("Retry-After"), time.Now()); delay > wait {
				wait = delay
			}
			if e := c.wait(ctx, wait); e != nil {
				return nil, nil, e
			}
			continue
		}
		msg := "request rejected"
		switch resp.StatusCode {
		case 401:
			msg = "credential rejected or expired"
		case 403:
			msg = "access denied; verify the NIP context and InvoiceRead permission"
		case 429:
			msg = "rate limit exceeded; retry later"
		}
		// Never echo arbitrary server error strings, which may contain secrets.
		var envelope struct {
			Exception struct {
				Details []struct {
					Code int `json:"exceptionCode"`
				} `json:"exceptionDetailList"`
			} `json:"exception"`
		}
		if json.Unmarshal(b, &envelope) == nil && len(envelope.Exception.Details) > 0 {
			msg += fmt.Sprintf(" (exception %d)", envelope.Exception.Details[0].Code)
		}
		return nil, resp.Header, &apiError{Status: resp.StatusCode, Message: msg}
	}
}
func retryAfter(value string, now time.Time) time.Duration {
	const maxDuration = time.Duration(1<<63 - 1)
	if seconds, err := strconv.ParseUint(value, 10, 64); err == nil {
		if seconds > uint64(maxDuration/time.Second) {
			return maxDuration
		}
		return time.Duration(seconds) * time.Second
	}
	if date, err := http.ParseTime(value); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}

func backoff(i int) time.Duration {
	if i > 4 {
		i = 4
	}
	return 250*time.Millisecond*time.Duration(1<<i) + time.Duration(randIntn(100))*time.Millisecond
}

var randIntn = func(n int) int {
	b := []byte{0}
	if _, e := rand.Read(b); e != nil {
		return 0
	}
	return int(b[0]) % n
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type cert struct {
	Certificate string    `json:"certificate"`
	PublicKeyID string    `json:"publicKeyId"`
	ValidFrom   time.Time `json:"validFrom"`
	ValidTo     time.Time `json:"validTo"`
	Usage       []string  `json:"usage"`
}
type challenge struct {
	Challenge   string `json:"challenge"`
	TimestampMs int64  `json:"timestampMs"`
}
type authStart struct {
	ReferenceNumber     string `json:"referenceNumber"`
	AuthenticationToken struct {
		Token string `json:"token"`
	} `json:"authenticationToken"`
}
type authStatus struct {
	Status struct {
		Code        int    `json:"code"`
		Description string `json:"description"`
	} `json:"status"`
}
type tokens struct {
	AccessToken struct {
		Token      string    `json:"token"`
		ValidUntil time.Time `json:"validUntil"`
	} `json:"accessToken"`
	RefreshToken struct {
		Token      string    `json:"token"`
		ValidUntil time.Time `json:"validUntil"`
	} `json:"refreshToken"`
}

func (c *Client) Authenticate(ctx context.Context, plain string) error {
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	b, _, e := c.request(ctx, "POST", "/auth/challenge", nil, "", 2)
	if e != nil {
		return e
	}
	var ch challenge
	if e = json.Unmarshal(b, &ch); e != nil {
		return fmt.Errorf("decode authentication challenge: %w", e)
	}
	if ch.Challenge == "" || ch.TimestampMs <= 0 {
		return errors.New("invalid authentication challenge")
	}
	b, _, e = c.request(ctx, "GET", "/security/public-key-certificates", nil, "", 3)
	if e != nil {
		return e
	}
	var certs []cert
	if e = json.Unmarshal(b, &certs); e != nil {
		return fmt.Errorf("decode public certificates: %w", e)
	}
	now := time.Now()
	var selected *cert
	var pub *rsa.PublicKey
	for i := range certs {
		candidate := &certs[i]
		if !contains(candidate.Usage, "KsefTokenEncryption") || now.Before(candidate.ValidFrom) || !now.Before(candidate.ValidTo) {
			continue
		}
		der, err := base64.StdEncoding.DecodeString(candidate.Certificate)
		if err != nil {
			continue
		}
		parsed, err := x509.ParseCertificate(der)
		if err != nil || now.Before(parsed.NotBefore) || !now.Before(parsed.NotAfter) {
			continue
		}
		key, ok := parsed.PublicKey.(*rsa.PublicKey)
		if !ok {
			continue
		}
		if selected == nil || candidate.ValidTo.After(selected.ValidTo) {
			selected, pub = candidate, key
		}
	}
	if selected == nil {
		return errors.New("no currently valid RSA KsefTokenEncryption certificate")
	}
	cipher, e := rsa.EncryptOAEP(sha256.New(), rand.Reader, pub, []byte(plain+"|"+strconv.FormatInt(ch.TimestampMs, 10)), nil)
	if e != nil {
		return e
	}
	payload := map[string]any{"challenge": ch.Challenge, "contextIdentifier": map[string]string{"type": "Nip", "value": c.nip}, "encryptedToken": base64.StdEncoding.EncodeToString(cipher), "publicKeyId": selected.PublicKeyID}
	b, _, e = c.request(ctx, "POST", "/auth/ksef-token", payload, "", 0)
	if e != nil {
		return e
	}
	var start authStart
	if e = json.Unmarshal(b, &start); e != nil {
		return fmt.Errorf("decode authentication start: %w", e)
	}
	authToken := start.AuthenticationToken.Token
	if authToken == "" || start.ReferenceNumber == "" {
		return errors.New("KSeF authentication response omitted token or reference")
	}
	for {
		if e = ctx.Err(); e != nil {
			return e
		}
		b, _, e = c.request(ctx, "GET", "/auth/"+start.ReferenceNumber, nil, authToken, 3)
		if e != nil {
			return e
		}
		var s authStatus
		if e = json.Unmarshal(b, &s); e != nil {
			return fmt.Errorf("decode authentication status: %w", e)
		}
		if s.Status.Code == 200 {
			break
		}
		if s.Status.Code != 100 {
			return fmt.Errorf("KSeF authentication failed (status %d)", s.Status.Code)
		}
		if e = c.wait(ctx, 750*time.Millisecond); e != nil {
			return e
		}
	}
	b, _, e = c.request(ctx, "POST", "/auth/token/redeem", nil, authToken, 0)
	if e != nil {
		return e
	}
	var ts tokens
	if e = json.Unmarshal(b, &ts); e != nil {
		return fmt.Errorf("decode redeemed tokens: %w", e)
	}
	if ts.AccessToken.Token == "" || ts.RefreshToken.Token == "" || !ts.AccessToken.ValidUntil.After(time.Now()) || !ts.RefreshToken.ValidUntil.After(time.Now()) {
		return errors.New("KSeF token redemption returned empty or expired tokens")
	}
	c.access = ts.AccessToken.Token
	c.refresh = ts.RefreshToken.Token
	c.accessExpiry = ts.AccessToken.ValidUntil
	c.refreshExpiry = ts.RefreshToken.ValidUntil
	return nil
}
func contains(a []string, s string) bool {
	for _, v := range a {
		if v == s {
			return true
		}
	}
	return false
}
func (c *Client) refreshTokens(ctx context.Context) error {
	if c.refresh == "" || (!c.refreshExpiry.IsZero() && !c.refreshExpiry.After(time.Now())) {
		return errors.New("refresh credential missing or expired; authenticate again")
	}
	b, _, e := c.request(ctx, "POST", "/auth/token/refresh", nil, c.refresh, 0)
	if e != nil {
		return e
	}
	var t tokens
	if e = json.Unmarshal(b, &t); e != nil {
		return e
	}
	if t.AccessToken.Token == "" || !t.AccessToken.ValidUntil.After(time.Now()) {
		return errors.New("refresh returned empty or expired access token")
	}
	c.access = t.AccessToken.Token
	c.accessExpiry = t.AccessToken.ValidUntil
	if t.RefreshToken.Token != "" {
		c.refresh = t.RefreshToken.Token
		c.refreshExpiry = t.RefreshToken.ValidUntil
	}
	return nil
}
func (c *Client) do(ctx context.Context, m, p string, body any) ([]byte, http.Header, error) {
	refreshed := false
	if !c.accessExpiry.IsZero() && !c.accessExpiry.After(time.Now().Add(30*time.Second)) {
		if err := c.refreshTokens(ctx); err != nil {
			return nil, nil, err
		}
		refreshed = true
	}
	b, h, e := c.request(ctx, m, p, body, c.access, 3)
	if ae, ok := e.(*apiError); ok && ae.Status == 401 && !refreshed {
		if re := c.refreshTokens(ctx); re != nil {
			return nil, nil, re
		}
		return c.request(ctx, m, p, body, c.access, 0)
	}
	return b, h, e
}

type Invoice struct {
	KSeFNumber           string    `json:"ksefNumber"`
	PermanentStorageDate time.Time `json:"permanentStorageDate"`
	Hash                 string    `json:"invoiceHash"`
}
type MetadataResponse struct {
	HasMore                 bool       `json:"hasMore"`
	IsTruncated             bool       `json:"isTruncated"`
	Invoices                []Invoice  `json:"invoices"`
	PermanentStorageHWMDate *time.Time `json:"permanentStorageHwmDate"`
}

func (c *Client) Query(ctx context.Context, subject string, from, to time.Time, offset int) (MetadataResponse, error) {
	subjectType := "Subject2"
	if subject == "issued" {
		subjectType = "Subject1"
	}
	body := map[string]any{"subjectType": subjectType, "dateRange": map[string]any{"dateType": "PermanentStorage", "from": from.Format(time.RFC3339Nano), "to": to.Format(time.RFC3339Nano), "restrictToPermanentStorageHwmDate": true}}
	path := fmt.Sprintf("/invoices/query/metadata?sortOrder=Asc&pageOffset=%d&pageSize=250", offset)
	b, _, e := c.do(ctx, "POST", path, body)
	if e != nil {
		return MetadataResponse{}, e
	}
	var r MetadataResponse
	if e = json.Unmarshal(b, &r); e != nil {
		return r, fmt.Errorf("decode invoice metadata: %w", e)
	}
	return r, nil
}
func (c *Client) Download(ctx context.Context, n string) ([]byte, error) {
	b, _, e := c.do(ctx, "GET", "/invoices/ksef/"+n, nil)
	return b, e
}
