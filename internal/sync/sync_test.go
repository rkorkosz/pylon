package sync

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
)

var startDate = time.Date(2025, 1, 1, 0, 0, 0, 123456789, time.UTC)

func id(i int) string { return fmt.Sprintf("5260250995-20250101-%012X-00", i) }
func xmlBytes(n string) []byte {
	return []byte("<?xml version=\"1.0\"?>\n<Invoice>" + n + "</Invoice>\n")
}
func storeFor(t *testing.T) *storage.Store {
	t.Helper()
	s, e := storage.Open(t.TempDir(), "test", "5260250995")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type fake struct {
	query            func(context.Context, string, time.Time, time.Time, int) (ksef.MetadataResponse, error)
	download         func(context.Context, string) ([]byte, error)
	calls, downloads int
}

func (f *fake) Query(ctx context.Context, s string, from, to time.Time, off int) (ksef.MetadataResponse, error) {
	f.calls++
	return f.query(ctx, s, from, to, off)
}
func (f *fake) Download(ctx context.Context, n string) ([]byte, error) {
	f.downloads++
	if f.download != nil {
		return f.download(ctx, n)
	}
	return xmlBytes(n), nil
}
func dataset(t *testing.T, invoices []ksef.Invoice, pageSize, limit int) *fake {
	t.Helper()
	return &fake{query: func(_ context.Context, _ string, from, to time.Time, offset int) (ksef.MetadataResponse, error) {
		if to.Sub(from) > 100*24*time.Hour || from.After(to) {
			t.Fatalf("invalid query window %s..%s", from, to)
		}
		var matches []ksef.Invoice
		for _, inv := range invoices {
			if !inv.PermanentStorageDate.Before(from) && !inv.PermanentStorageDate.After(to) {
				matches = append(matches, inv)
			}
		}
		lo := offset * pageSize
		hi := min(lo+pageSize, len(matches), limit)
		if lo > hi {
			return ksef.MetadataResponse{}, errors.New("invalid page offset")
		}
		return ksef.MetadataResponse{Invoices: matches[lo:hi], HasMore: hi < len(matches), IsTruncated: hi == limit && hi < len(matches), PermanentStorageHWMDate: &to}, nil
	}}
}
func run(t *testing.T, f Downloader, s *storage.Store, from, to *time.Time, subjects ...string) ([]Summary, error) {
	t.Helper()
	return Run(context.Background(), f, s, "test", "5260250995", subjects, from, to)
}
func checkpointAt(t *testing.T, s *storage.Store, subject string, want *time.Time) {
	t.Helper()
	cp, ok, e := s.Checkpoint(subject)
	if e != nil {
		t.Fatal(e)
	}
	if want == nil {
		if ok {
			t.Fatalf("unfinished window checkpointed: %s", cp.Cursor)
		}
		return
	}
	if !ok || !cp.Cursor.Equal(*want) {
		t.Fatalf("checkpoint %s, want %s", cp.Cursor, *want)
	}
}
func TestRunPagesWindowsAndEmpty(t *testing.T) {
	end := startDate.Add(201 * 24 * time.Hour)
	invs := []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}, {KSeFNumber: id(2), PermanentStorageDate: startDate.Add(time.Hour)}, {KSeFNumber: id(3), PermanentStorageDate: startDate.Add(100 * 24 * time.Hour)}}
	s := storeFor(t)
	f := dataset(t, invs, 1, 10000)
	got, e := run(t, f, s, &startDate, &end, "received")
	if e != nil {
		t.Fatal(e)
	}
	if got[0].Downloaded != 3 || got[0].AlreadyPresent != 1 || got[0].Failed != 0 {
		t.Fatalf("summary %#v", got)
	}
	checkpointAt(t, s, "received", &end)
	for _, inv := range invs {
		if ok, e := s.ExistingValid(inv.KSeFNumber); e != nil || !ok {
			t.Fatal(inv.KSeFNumber, ok, e)
		}
	}
	before := f.calls
	if _, e = run(t, f, s, nil, &end, "received"); e != nil || f.calls != before {
		t.Fatal("completed repeat queried", e)
	}
	// Earlier replay preserves the saved cursor and downloads no extra XML.
	earlyEnd := startDate.Add(2 * time.Hour)
	got, e = run(t, f, s, &startDate, &earlyEnd, "received")
	if e != nil || got[0].Downloaded != 0 {
		t.Fatal(got, e)
	}
	checkpointAt(t, s, "received", &end)
}
func TestTruncationOverTenThousand(t *testing.T) {
	const count = 10001
	invs := make([]ksef.Invoice, count)
	for i := range invs {
		invs[i] = ksef.Invoice{KSeFNumber: id(i + 1), PermanentStorageDate: startDate.Add(time.Duration(i+1) * time.Second)}
	}
	end := startDate.Add(24 * time.Hour)
	s := storeFor(t)
	f := dataset(t, invs, 250, 10000)
	got, e := run(t, f, s, &startDate, &end, "received")
	if e != nil {
		t.Fatal(e)
	}
	if got[0].Downloaded != count || f.downloads != count {
		t.Fatal("lost or redownloaded invoices", got, f.downloads)
	}
	checkpointAt(t, s, "received", &end)
	files, e := filepath.Glob(filepath.Join(s.Root, "test", "5260250995", "invoices", "*.xml"))
	if e != nil || len(files) != count {
		t.Fatal("incorrect unique XML set", len(files), e)
	}
}
func TestEqualTimestampNoProgress(t *testing.T) {
	d := startDate.Add(time.Hour)
	end := d.Add(time.Hour)
	invs := []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: d}, {KSeFNumber: id(2), PermanentStorageDate: d}, {KSeFNumber: id(3), PermanentStorageDate: d}}
	s := storeFor(t)
	f := dataset(t, invs, 2, 2)
	got, e := run(t, f, s, &startDate, &end, "received")
	if e == nil || f.calls > 3 || got[0].Failed != 1 {
		t.Fatal(got, e, f.calls)
	}
	checkpointAt(t, s, "received", nil)
}
func TestWatermarks(t *testing.T) {
	for _, scenario := range []string{"missing first", "missing later", "regressing", "advancing", "lagging", "behind start"} {
		t.Run(scenario, func(t *testing.T) {
			s := storeFor(t)
			end := startDate.Add(201 * 24 * time.Hour)
			stable := startDate.Add(time.Hour)
			calls := 0
			f := &fake{query: func(_ context.Context, _ string, from, to time.Time, off int) (ksef.MetadataResponse, error) {
				calls++
				h := stable
				more := calls == 1
				if calls > 1 && !to.Equal(stable) {
					t.Errorf("upper bound not fixed: %s, want %s", to, stable)
				}
				switch scenario {
				case "missing first":
					return ksef.MetadataResponse{}, nil
				case "missing later":
					if calls > 1 {
						return ksef.MetadataResponse{}, nil
					}
				case "regressing":
					if calls > 1 {
						h = h.Add(-time.Second)
					}
				case "advancing":
					if calls > 1 {
						h = h.Add(time.Hour)
					}
				case "lagging":
					more = false
				case "behind start":
					h = startDate.Add(-time.Second)
					more = false
				}
				var invs []ksef.Invoice
				if more {
					invs = []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}}
				}
				return ksef.MetadataResponse{Invoices: invs, HasMore: more, PermanentStorageHWMDate: &h}, nil
			}}
			got, e := run(t, f, s, &startDate, &end, "received")
			if e == nil {
				t.Fatal("deferred or invalid window reported complete")
			}
			if scenario == "advancing" || scenario == "lagging" {
				checkpointAt(t, s, "received", &stable)
				if !got[0].DeferredFrom.Equal(stable) || !got[0].DeferredTo.Equal(end) {
					t.Fatal("deferred interval incomplete", got)
				}
			} else {
				checkpointAt(t, s, "received", nil)
			}
		})
	}
}
func TestFailureAtEveryDocumentAndRestart(t *testing.T) {
	end := startDate.Add(time.Hour)
	invs := []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}, {KSeFNumber: id(2), PermanentStorageDate: startDate}, {KSeFNumber: id(3), PermanentStorageDate: startDate}}
	for failAt := 1; failAt <= 3; failAt++ {
		t.Run(fmt.Sprint(failAt), func(t *testing.T) {
			s := storeFor(t)
			f := dataset(t, invs, 2, 10000)
			f.download = func(_ context.Context, n string) ([]byte, error) {
				if n == id(failAt) {
					return nil, errors.New("injected failure")
				}
				return xmlBytes(n), nil
			}
			got, e := run(t, f, s, &startDate, &end, "received")
			if e == nil || got[0].Downloaded != failAt-1 || got[0].Failed != 1 {
				t.Fatal(got, e)
			}
			checkpointAt(t, s, "received", nil)
			f.download = nil
			got, e = run(t, f, s, &startDate, &end, "received")
			if e != nil || got[0].AlreadyPresent != failAt-1 {
				t.Fatal(got, e)
			}
			checkpointAt(t, s, "received", &end)
		})
	}
}
func TestFailureSummaryAfterCompletedWindow(t *testing.T) {
	s := storeFor(t)
	end := startDate.Add(101 * 24 * time.Hour)
	firstEnd := startDate.Add(100 * 24 * time.Hour)
	invs := []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}, {KSeFNumber: id(2), PermanentStorageDate: firstEnd.Add(time.Hour)}}
	f := dataset(t, invs, 250, 10000)
	f.download = func(_ context.Context, n string) ([]byte, error) {
		if n == id(2) {
			return nil, errors.New("failure")
		}
		return xmlBytes(n), nil
	}
	got, e := run(t, f, s, &startDate, &end, "received")
	if e == nil || got[0].Failed != 1 || got[0].Downloaded != 1 || !got[0].CompletedThrough.Equal(firstEnd) {
		t.Fatal(got, e)
	}
	checkpointAt(t, s, "received", &firstEnd)
}
func TestCheckpointCommitFailureAndRestart(t *testing.T) {
	s := storeFor(t)
	end := startDate.Add(time.Hour)
	cpPath := filepath.Join(s.Root, "test", "5260250995", "checkpoints", "received.json")
	f := dataset(t, []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}}, 250, 10000)
	f.download = func(_ context.Context, n string) ([]byte, error) {
		if e := os.Mkdir(cpPath, 0700); e != nil {
			t.Fatal(e)
		}
		return xmlBytes(n), nil
	}
	got, e := run(t, f, s, &startDate, &end, "received")
	if e == nil || got[0].Downloaded != 1 || !got[0].CompletedThrough.IsZero() {
		t.Fatal(got, e)
	}
	if e = os.Remove(cpPath); e != nil {
		t.Fatal(e)
	}
	checkpointAt(t, s, "received", nil)
	f.download = nil
	got, e = run(t, f, s, &startDate, &end, "received")
	if e != nil || got[0].AlreadyPresent != 1 || got[0].Downloaded != 0 {
		t.Fatal(got, e)
	}
	checkpointAt(t, s, "received", &end)
}
func TestSubjectsShareFilesAndKeepIndependentProgress(t *testing.T) {
	s := storeFor(t)
	end := startDate.Add(time.Hour)
	f := dataset(t, []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}}, 250, 10000)
	got, e := run(t, f, s, &startDate, &end, "received", "issued")
	if e != nil || got[0].Downloaded != 1 || got[1].AlreadyPresent != 1 {
		t.Fatal(got, e)
	}
	for _, subject := range []string{"received", "issued"} {
		checkpointAt(t, s, subject, &end)
	}
	// Failure in issued cannot disturb received progress.
	later := end.Add(time.Hour)
	f.query = func(_ context.Context, subject string, _, to time.Time, _ int) (ksef.MetadataResponse, error) {
		if subject == "issued" {
			return ksef.MetadataResponse{}, errors.New("issued failure")
		}
		return ksef.MetadataResponse{PermanentStorageHWMDate: &to}, nil
	}
	got, e = run(t, f, s, nil, &later, "received", "issued")
	if e == nil || len(got) != 2 || got[1].Failed != 1 {
		t.Fatal(got, e)
	}
	checkpointAt(t, s, "received", &later)
	checkpointAt(t, s, "issued", &end)
}
func TestCancellationAndInvalidPagination(t *testing.T) {
	for _, mode := range []string{"cancel query", "cancel download", "empty page", "repeated page", "out of bounds"} {
		t.Run(mode, func(t *testing.T) {
			s := storeFor(t)
			end := startDate.Add(time.Hour)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			f := &fake{query: func(_ context.Context, _ string, _, to time.Time, off int) (ksef.MetadataResponse, error) {
				if mode == "cancel query" {
					cancel()
				}
				invs := []ksef.Invoice{{KSeFNumber: id(1), PermanentStorageDate: startDate}}
				if mode == "empty page" {
					invs = nil
				}
				if mode == "out of bounds" {
					invs[0].PermanentStorageDate = end.Add(time.Second)
				}
				return ksef.MetadataResponse{Invoices: invs, HasMore: slices.Contains([]string{"empty page", "repeated page"}, mode), PermanentStorageHWMDate: &to}, nil
			}, download: func(_ context.Context, n string) ([]byte, error) {
				if mode == "cancel download" {
					cancel()
				}
				return xmlBytes(n), nil
			}}
			_, e := Run(ctx, f, s, "test", "5260250995", []string{"received"}, &startDate, &end)
			if e == nil {
				t.Fatal("invalid/cancelled sync succeeded")
			}
			if mode == "cancel query" || mode == "cancel download" {
				if !errors.Is(e, context.Canceled) {
					t.Fatal(e)
				}
			}
			if f.calls > 2 {
				t.Fatal("pagination did not terminate")
			}
			checkpointAt(t, s, "received", nil)
		})
	}
}
