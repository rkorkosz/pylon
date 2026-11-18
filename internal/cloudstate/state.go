// Package cloudstate stores received-invoice coverage and the Google Tasks outbox.
package cloudstate

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/rkrokosz/pylon/internal/invoice"
	"github.com/rkrokosz/pylon/internal/storage"
)

const (
	Pending    = "pending"
	Sending    = "dispatching"
	Unresolved = "unresolved"
	Created    = "created"
	Skipped    = "skipped"
)

var ErrLease = errors.New("run lease busy or no longer owned")

// Intent never contains raw invoice XML. ID is an opaque reconciliation marker.
type Intent struct {
	ID        string `firestore:"-"`
	Status    string `firestore:"status"`
	Reference string `firestore:"reference,omitempty"`
	Deadline  string `firestore:"deadline,omitempty"`
	TaskID    string `firestore:"taskId,omitempty"`
	Claim     string `firestore:"claim,omitempty"`
	Reason    string `firestore:"reason,omitempty"`
	Digest    string `firestore:"digest"`
	StoredAt  string `firestore:"storedAt"`
}

func key(parts ...string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(strings.Join(parts, "\x00"))))
}

func MonthStart(now time.Time, warsaw *time.Location) time.Time {
	y, m, _ := now.In(warsaw).Date()
	return time.Date(y, m, 1, 0, 0, 0, 0, warsaw).UTC()
}

func Prepare(ctx context.Context, n string, b []byte, m storage.Metadata) (Intent, error) {
	if err := checkMetadata(n, m); err != nil {
		return Intent{}, err
	}
	digest, err := storage.ValidateDocument(ctx, b, m.APIHash)
	if err != nil {
		return Intent{}, err
	}
	due, _, ok, err := invoice.PaymentDate(ctx, b)
	if err != nil {
		return Intent{}, err
	}
	i := Intent{Digest: base64.StdEncoding.EncodeToString(digest[:]), StoredAt: m.PermanentStorageDate.UTC().Format(time.RFC3339Nano)}
	if !ok {
		i.Status, i.Reason = Skipped, "unsupported_terms"
		return i, nil
	}
	// The first segment is the issuer's NIP. Keep only the standard reference
	// suffix, not the NIP or the seller's free-text invoice number.
	_, reference, _ := strings.Cut(n, "-")
	i.Status, i.Reference, i.Deadline = Pending, reference, due.Format(time.DateOnly)
	return i, nil
}

func checkMetadata(n string, m storage.Metadata) error {
	if m.Subject != "received" || m.PermanentStorageDate.IsZero() {
		return errors.New("invalid received invoice metadata")
	}
	return storage.ValidateNumber(n)
}

func (i Intent) verify(m storage.Metadata) error {
	if i.StoredAt != m.PermanentStorageDate.UTC().Format(time.RFC3339Nano) || (m.APIHash != "" && i.Digest != m.APIHash) {
		return errors.New("metadata conflicts with staged invoice")
	}
	return nil
}
