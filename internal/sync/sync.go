package sync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
)

type Summary struct {
	Subject                                        string
	Discovered, Downloaded, AlreadyPresent, Failed int
	Skipped                                        int
	CompletedThrough                               time.Time
	DeferredFrom, DeferredTo                       time.Time
}
type Downloader interface {
	Query(context.Context, string, time.Time, time.Time, int) (ksef.MetadataResponse, error)
	Download(context.Context, string) ([]byte, error)
}

// Persistence must durably handle each invoice before SaveCursor succeeds.
type Persistence interface {
	Cursor(context.Context, string) (time.Time, bool, error)
	SaveCursor(context.Context, string, time.Time) error
	Existing(context.Context, string, storage.Metadata) (bool, error)
	Save(context.Context, string, []byte, storage.Metadata) (policySkipped bool, err error)
}

type localStore struct {
	*storage.Store
	env, nip string
}

func (l localStore) Cursor(_ context.Context, subject string) (time.Time, bool, error) {
	cp, ok, err := l.Checkpoint(subject)
	return cp.Cursor, ok, err
}
func (l localStore) SaveCursor(ctx context.Context, subject string, cursor time.Time) error {
	return l.SaveCheckpointContext(ctx, storage.Checkpoint{Environment: l.env, NIP: l.nip, Subject: subject, Cursor: cursor})
}
func (l localStore) Existing(ctx context.Context, n string, m storage.Metadata) (bool, error) {
	return l.ExistingValidContext(ctx, n, m)
}
func (l localStore) Save(ctx context.Context, n string, b []byte, m storage.Metadata) (bool, error) {
	_, err := l.SaveXMLContext(ctx, n, b, m)
	return false, err
}

func Run(ctx context.Context, api Downloader, store *storage.Store, env, nip string, subjects []string, from, to *time.Time) ([]Summary, error) {
	var out []Summary
	end := time.Now().UTC()
	if to != nil {
		end = *to
	}
	for _, subject := range subjects {
		s, e := runSubject(ctx, api, localStore{store, env, nip}, subject, from, end, 100*24*time.Hour)
		out = append(out, s)
		if e != nil {
			return out, e
		}
	}
	return out, nil
}

// Received uses shorter checkpoint windows for bounded cloud invocations.
func Received(ctx context.Context, api Downloader, store Persistence, end time.Time) (Summary, error) {
	return runSubject(ctx, api, store, "received", nil, end, 24*time.Hour)
}

func runSubject(ctx context.Context, api Downloader, store Persistence, subject string, from *time.Time, end time.Time, window time.Duration) (s Summary, err error) {
	s.Subject = subject
	defer func() {
		if err != nil && s.DeferredTo.IsZero() {
			s.Failed++
		}
	}()
	saved, exists, err := store.Cursor(ctx, subject)
	if err != nil {
		return s, err
	}
	if exists {
		s.CompletedThrough = saved
	}
	start := saved
	if !exists {
		if from == nil {
			return s, fmt.Errorf("first sync for %s requires --from", subject)
		}
		start = *from
	}
	if from != nil && from.Before(start) {
		start = *from
	}
	if err = ctx.Err(); err != nil {
		return s, err
	}
	for cursor := start; cursor.Before(end); {
		requestedEnd := cursor.Add(window)
		if requestedEnd.After(end) {
			requestedEnd = end
		}
		effectiveEnd := requestedEnd
		boundary := cursor
		offset, pages := 0, 0
		lastHWM := time.Time{}
		processed := map[string]bool{}
		for {
			if err = ctx.Err(); err != nil {
				return s, err
			}
			r, e := api.Query(ctx, subject, boundary, effectiveEnd, offset)
			if e != nil {
				return s, fmt.Errorf("query %s invoices: %w", subject, e)
			}
			pages++
			if pages > 100000 {
				return s, errors.New("pagination exceeded safety limit")
			}
			if r.PermanentStorageHWMDate == nil || r.PermanentStorageHWMDate.IsZero() {
				s.DeferredFrom = cursor
				s.DeferredTo = end
				return s, fmt.Errorf("%s sync incomplete: KSeF omitted completeness watermark; window uncommitted", subject)
			}
			h := r.PermanentStorageHWMDate.UTC()
			if !lastHWM.IsZero() && h.Before(lastHWM) {
				return s, errors.New("KSeF completeness watermark regressed; window uncommitted")
			}
			if lastHWM.IsZero() && h.Before(effectiveEnd) {
				effectiveEnd = h
			}
			lastHWM = h
			if !effectiveEnd.After(cursor) {
				s.DeferredFrom = cursor
				s.DeferredTo = end
				return s, fmt.Errorf("%s sync incomplete: KSeF completeness watermark deferred coverage", subject)
			}
			newRecords := 0
			for _, inv := range r.Invoices {
				if inv.PermanentStorageDate.IsZero() || inv.PermanentStorageDate.Before(boundary) || inv.PermanentStorageDate.After(effectiveEnd) {
					return s, errors.New("metadata storage date outside stable query bounds")
				}
				s.Discovered++
				if processed[inv.KSeFNumber] {
					continue
				}
				processed[inv.KSeFNumber] = true
				newRecords++
				if err = ctx.Err(); err != nil {
					return s, err
				}
				m := storage.Metadata{PermanentStorageDate: inv.PermanentStorageDate, Subject: subject, APIHash: inv.Hash}
				present, e := store.Existing(ctx, inv.KSeFNumber, m)
				if e != nil {
					return s, e
				}
				if present {
					s.AlreadyPresent++
					continue
				}
				b, e := api.Download(ctx, inv.KSeFNumber)
				if e != nil {
					return s, fmt.Errorf("download invoice %s: %w", inv.KSeFNumber, e)
				}
				skipped, e := store.Save(ctx, inv.KSeFNumber, b, m)
				if e != nil {
					return s, fmt.Errorf("store invoice %s: %w", inv.KSeFNumber, e)
				}
				if skipped {
					s.Skipped++
				}
				s.Downloaded++
			}
			if !r.HasMore {
				break
			}
			if len(r.Invoices) == 0 || newRecords == 0 {
				return s, errors.New("metadata pagination made no progress; window uncommitted")
			}
			if r.IsTruncated {
				d := r.Invoices[len(r.Invoices)-1].PermanentStorageDate.UTC()
				if !d.After(boundary) {
					return s, errors.New("metadata truncation boundary made no timestamp progress; use bulk export")
				}
				boundary = d
				offset = 0
			} else {
				offset++
			}
		}
		if err = ctx.Err(); err != nil {
			return s, err
		}
		if !exists || effectiveEnd.After(saved) {
			if e := store.SaveCursor(ctx, subject, effectiveEnd); e != nil {
				return s, e
			}
			saved = effectiveEnd
			exists = true
		}
		s.CompletedThrough = saved
		cursor = effectiveEnd
		if effectiveEnd.Before(requestedEnd) {
			s.DeferredFrom = effectiveEnd
			s.DeferredTo = end
			return s, fmt.Errorf("%s sync incomplete: KSeF completeness watermark deferred coverage", subject)
		}
	}
	return s, nil
}
