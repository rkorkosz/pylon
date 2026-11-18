package cloudstate

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
	"github.com/rkrokosz/pylon/internal/sync"
)

const sampleNumber = "5260250995-20261001-ABCDEFABCDEF-AB"
const sampleXML = `<Faktura xmlns="http://crd.gov.pl/wzor/2025/06/25/13775/"><Fa><Platnosc><TerminPlatnosci><Termin>2026-11-17</Termin></TerminPlatnosci></Platnosc></Fa></Faktura>`

func TestPrepare(t *testing.T) {
	m := storage.Metadata{Subject: "received", PermanentStorageDate: time.Now()}
	for _, tc := range []struct{ n, b, hash, want string }{
		{sampleNumber, sampleXML, "", Pending},
		{sampleNumber, `<Faktura/>`, "", Skipped},
		{"invalid", sampleXML, "", ""},
		{sampleNumber, `<Faktura/><Extra/>`, "", ""},
		{sampleNumber, sampleXML, "wrong hash", ""},
	} {
		m.APIHash = tc.hash
		i, err := Prepare(t.Context(), tc.n, []byte(tc.b), m)
		if i.Status != tc.want || (err != nil) != (tc.want == "") {
			t.Fatal(i, err)
		}
		if tc.want == Pending && i.Reference != "20261001-ABCDEFABCDEF-AB" {
			t.Fatal("task reference should omit the issuer NIP", i.Reference)
		}
	}
}

type api struct {
	t             *testing.T
	stored, first time.Time
	hwm           *time.Time
	downloads     int
}

func (a *api) Query(_ context.Context, subject string, from, to time.Time, _ int) (r ksef.MetadataResponse, err error) {
	if subject != "received" {
		a.t.Fatalf("queried %s", subject)
	}
	if a.first.IsZero() {
		a.first = from
	}
	r.PermanentStorageHWMDate = a.hwm
	if a.hwm == nil {
		return r, nil
	}
	if a.stored.IsZero() {
		a.stored = from.Add(time.Hour)
	}
	if !a.stored.Before(from) && !a.stored.After(to) && !a.stored.After(*a.hwm) {
		r.Invoices = []ksef.Invoice{{KSeFNumber: sampleNumber, PermanentStorageDate: a.stored}}
	}
	return r, nil
}
func (a *api) Download(context.Context, string) ([]byte, error) {
	a.downloads++
	return []byte(sampleXML), nil
}

type rejectCheckpoint struct{ *Run }

func (rejectCheckpoint) SaveCursor(context.Context, string, time.Time) error {
	return errors.New("injected checkpoint failure")
}

func TestFirestoreReplayAndFencing(t *testing.T) {
	if os.Getenv("FIRESTORE_EMULATOR_HOST") == "" {
		t.Skip("requires the Firestore emulator; see docs/cloud-run.md")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	db, err := firestore.NewClient(ctx, "demo-pylon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 10, 31, 12, 0, 0, 0, time.UTC)
	after := before.Add(36 * time.Hour)
	start := MonthStart(before, warsaw)
	deployment := rand.Text()
	acquire := func(now time.Time, env string) (*Run, error) {
		return Acquire(ctx, db, deployment, env, "5260250995", "list", MonthStart(now, warsaw), time.Now().Add(time.Minute))
	}
	a, err := acquire(before, "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquire(after, "test"); !errors.Is(err, ErrLease) {
		t.Fatalf("second lease: %v", err)
	}
	other, err := acquire(after, "prod")
	if err != nil {
		t.Fatal("TEST and prod share a lease", err)
	}
	if err := other.Release(ctx); err != nil {
		t.Fatal(err)
	}
	upstream := &api{t: t, hwm: &before}
	if _, err := sync.Received(ctx, upstream, rejectCheckpoint{a}, before); err == nil {
		t.Fatal("checkpoint unexpectedly succeeded")
	}
	if cursor, _, err := a.Cursor(ctx, "received"); err != nil || !cursor.Equal(start) || upstream.downloads != 1 {
		t.Fatal(cursor, err, upstream.downloads)
	}
	var ids []string
	if err := a.Work(ctx, func(i Intent) error { ids = append(ids, i.ID); return nil }); err != nil || len(ids) != 1 {
		t.Fatal(ids, err)
	}
	if _, insert, err := a.Claim(ctx, ids[0]); err != nil || !insert {
		t.Fatal(insert, err)
	}
	if _, _, err := a.Claim(ctx, ids[0]); err == nil {
		t.Fatal("double claim")
	}
	if err := a.Resolve(ctx, ids[0], Created, "task-ID", ""); err != nil {
		t.Fatal(err)
	}
	// Advance the emulator lease expiry to simulate a crashed revision.
	if _, err := a.root.Update(ctx, []firestore.Update{{Path: "expires", Value: time.Now().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
	b, err := acquire(after, "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := a.SaveCursor(ctx, "received", after); !errors.Is(err, ErrLease) {
		t.Fatalf("stale cursor write: %v", err)
	}
	if err := a.Resolve(ctx, ids[0], Pending, "", ""); !errors.Is(err, ErrLease) {
		t.Fatalf("stale intent write: %v", err)
	}
	upstream.first = time.Time{}
	upstream.hwm = &before
	if _, err := sync.Received(ctx, upstream, b, after); err == nil {
		t.Fatal("lagging HWM reported complete")
	}
	if !upstream.first.Equal(start) || upstream.downloads != 1 {
		t.Fatal("bootstrap reset or invoice re-downloaded", upstream.first, upstream.downloads)
	}
	if cursor, _, err := b.Cursor(ctx, "received"); err != nil || !cursor.Equal(before) {
		t.Fatal(cursor, err)
	}
	upstream.first = time.Time{}
	upstream.hwm = nil
	if _, err := sync.Received(ctx, upstream, b, after); err == nil {
		t.Fatal("missing HWM reported complete")
	}
	if cursor, _, err := b.Cursor(ctx, "received"); err != nil || !cursor.Equal(before) {
		t.Fatal(cursor, err)
	}
	upstream.hwm = &after
	if _, err := sync.Received(ctx, upstream, b, after); err != nil {
		t.Fatal(err)
	}
	if !upstream.first.Equal(before) {
		t.Fatal("normal cross-month run skipped prior month", upstream.first)
	}
	var intent Intent
	if ok, err := read(func(ref *firestore.DocumentRef) (*firestore.DocumentSnapshot, error) { return ref.Get(ctx) }, b.intents().Doc(ids[0]), &intent); err != nil || !ok || intent.TaskID != "task-ID" || intent.Status != Created {
		t.Fatal(intent, ok, err)
	}
	if err := b.Work(ctx, func(i Intent) error { return errors.New("created intent must not be redispatched") }); err != nil {
		t.Fatal(err)
	}
}
