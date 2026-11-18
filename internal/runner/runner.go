// Package runner coordinates discovery and dispatch without sharing failures between APIs.
package runner

import (
	"context"
	"errors"
	"time"

	"github.com/rkrokosz/pylon/internal/cloudstate"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/sync"
	"github.com/rkrokosz/pylon/internal/tasks"
)

type State interface {
	sync.Persistence
	Work(context.Context, func(cloudstate.Intent) error) error
	Claim(context.Context, string) (cloudstate.Intent, bool, error)
	Resolve(context.Context, string, string, string, string) error
}

type Tasks interface {
	Insert(context.Context, tasks.Task) (string, bool, error)
	Marked(context.Context) (map[string][]tasks.Task, error)
}

type Source func(context.Context) (sync.Downloader, error)

type Result struct {
	Discoveries        int  `json:"discoveries"`
	Staged             int  `json:"staged"`
	Skipped            int  `json:"skipped"`
	Created            int  `json:"tasks_created"`
	Recovered          int  `json:"tasks_recovered"`
	PaidRecovered      int  `json:"paid_tasks_recovered"`
	Deferred           int  `json:"deferred_windows"`
	Unresolved         int  `json:"unresolved_intents"`
	DiscoveryError     bool `json:"discovery_incomplete"`
	DispatchError      bool `json:"dispatch_incomplete"`
	AuthorizationError bool `json:"authorization_error"`
}

func Run(ctx context.Context, state State, source Source, dest Tasks, end time.Time, discoveryBudget time.Duration, warsaw *time.Location, now func() time.Time) Result {
	var out Result
	// A slow or failing KSeF request cannot consume the entire dispatch budget.
	discovery, cancel := context.WithTimeout(ctx, discoveryBudget)
	api, err := source(discovery)
	if err == nil {
		summary, err := sync.Received(discovery, api, state, end)
		out.Discoveries, out.Staged, out.Skipped = summary.Discovered, summary.Downloaded, summary.Skipped
		out.DiscoveryError = err != nil
		if !summary.DeferredTo.IsZero() {
			out.Deferred++
		}
		out.AuthorizationError = errors.Is(err, ksef.ErrAuthorization)
	} else {
		out.DiscoveryError = true
		out.AuthorizationError = errors.Is(err, ksef.ErrAuthorization)
	}
	cancel()
	if err := dispatch(ctx, state, dest, warsaw, now, &out); err != nil {
		out.DispatchError = true
		out.AuthorizationError = out.AuthorizationError || errors.Is(err, tasks.ErrAuthorization)
	}
	return out
}

func (r Result) Complete() bool {
	return !r.DiscoveryError && !r.DispatchError && !r.AuthorizationError && r.Unresolved == 0
}

func dispatch(ctx context.Context, state State, dest Tasks, warsaw *time.Location, now func() time.Time, out *Result) error {
	var scan map[string][]tasks.Task
	return state.Work(ctx, func(candidate cloudstate.Intent) error {
		i, insert, err := state.Claim(ctx, candidate.ID)
		if err != nil {
			return err
		}
		if !insert {
			if scan == nil {
				scan, err = dest.Marked(ctx)
				if err != nil {
					out.Unresolved++
					return errors.Join(err, state.Resolve(ctx, i.ID, cloudstate.Unresolved, "", "reconciliation_unavailable"))
				}
			}
			matches := scan[tasks.Marker(i.ID)]
			if len(matches) != 1 {
				out.Unresolved++
				return state.Resolve(ctx, i.ID, cloudstate.Unresolved, "", "no_unique_marker")
			}
			if err := state.Resolve(ctx, i.ID, cloudstate.Created, matches[0].ID, ""); err != nil {
				return err
			}
			out.Recovered++
			if matches[0].Paid() {
				out.PaidRecovered++
			}
			return nil
		}

		today := now().In(warsaw)
		due, err := DueDate(i.Deadline, today)
		if err != nil {
			return err
		}
		if due == "" {
			if err := state.Resolve(ctx, i.ID, cloudstate.Skipped, "", "deadline_passed"); err != nil {
				return err
			}
			out.Skipped++
			return nil
		}
		dateCheck := tasks.WithDateCheck(ctx, func() bool { return now().In(warsaw).Format(time.DateOnly) == today.Format(time.DateOnly) })
		id, unknown, err := dest.Insert(dateCheck, tasks.Task{Title: "Invoice due: " + i.Reference, Notes: "Payment deadline: " + i.Deadline + "\n" + tasks.Marker(i.ID), Due: due + "T00:00:00Z"})
		if unknown || (err == nil && id == "") {
			out.Unresolved++
			return state.Resolve(ctx, i.ID, cloudstate.Unresolved, "", "insert_unknown")
		}
		if err != nil {
			return errors.Join(err, state.Resolve(ctx, i.ID, cloudstate.Pending, "", "insert_rejected"))
		}
		if err := state.Resolve(ctx, i.ID, cloudstate.Created, id, ""); err != nil {
			out.Unresolved++
			return err
		}
		out.Created++
		return nil
	})
}

// DueDate uses UTC only as a representation of a date, not to reinterpret a
// Warsaw midnight as an instant. Empty means the payment deadline has passed.
func DueDate(deadline string, localNow time.Time) (string, error) {
	d, err := time.Parse(time.DateOnly, deadline)
	if err != nil {
		return "", err
	}
	y, m, day := localNow.Date()
	today := time.Date(y, m, day, 0, 0, 0, 0, time.UTC)
	if d.Before(today) {
		return "", nil
	}
	return maxDate(d.AddDate(0, 0, -1), today).Format(time.DateOnly), nil
}

func maxDate(a, b time.Time) time.Time {
	if a.Before(b) {
		return b
	}
	return a
}
