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
	CompletedThrough                               time.Time
	DeferredFrom, DeferredTo                       time.Time
}
type Downloader interface {
	Query(context.Context, string, time.Time, time.Time, int) (ksef.MetadataResponse, error)
	Download(context.Context, string) ([]byte, error)
}

func Run(ctx context.Context, api Downloader, store *storage.Store, env, nip string, subjects []string, from, to *time.Time) ([]Summary, error) {
	var out []Summary
	end := time.Now().UTC()
	if to != nil {
		end = *to
	}
	for _, subject := range subjects {
		s, e := runSubject(ctx, api, store, env, nip, subject, from, end)
		out = append(out, s)
		if e != nil {
			return out, e
		}
	}
	return out, nil
}
func runSubject(ctx context.Context, api Downloader, store *storage.Store, env, nip, subject string, from *time.Time, end time.Time) (s Summary, err error) {
	s.Subject = subject
	defer func() {
		if err != nil && s.DeferredTo.IsZero() {
			s.Failed++
		}
	}()
	cp, exists, err := store.Checkpoint(subject)
	if err != nil {
		return s, err
	}
	if exists {
		s.CompletedThrough = cp.Cursor
	}
	start := cp.Cursor
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
		requestedEnd := cursor.Add(100 * 24 * time.Hour)
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
				present, e := store.ExistingValid(inv.KSeFNumber, m)
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
				if _, e = store.SaveXMLContext(ctx, inv.KSeFNumber, b, m); e != nil {
					return s, fmt.Errorf("store invoice %s: %w", inv.KSeFNumber, e)
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
		if !exists || effectiveEnd.After(cp.Cursor) {
			if e := store.SaveCheckpoint(storage.Checkpoint{Environment: env, NIP: nip, Subject: subject, Cursor: effectiveEnd}); e != nil {
				return s, e
			}
			cp.Cursor = effectiveEnd
			exists = true
		}
		s.CompletedThrough = cp.Cursor
		cursor = effectiveEnd
		if effectiveEnd.Before(requestedEnd) {
			s.DeferredFrom = effectiveEnd
			s.DeferredTo = end
			return s, fmt.Errorf("%s sync incomplete: KSeF completeness watermark deferred coverage", subject)
		}
	}
	return s, nil
}
