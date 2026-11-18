package runner

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/rkrokosz/pylon/internal/cloudstate"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/sync"
	"github.com/rkrokosz/pylon/internal/tasks"
)

func TestDueDate(t *testing.T) {
	warsaw, err := time.LoadLocation("Europe/Warsaw")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ deadline, now, want string }{
		{"2027-01-01", "2026-12-29T11:00:00Z", "2026-12-31"},
		{"2028-03-01", "2028-02-27T11:00:00Z", "2028-02-29"},
		{"2026-03-30", "2026-03-27T11:00:00Z", "2026-03-29"},
		{"2026-10-26", "2026-10-21T11:00:00Z", "2026-10-25"},
		{"2026-06-01", "2026-05-31T22:00:00Z", "2026-06-01"},
		{"2026-05-31", "2026-05-31T22:00:00Z", ""},
	} {
		now, err := time.Parse(time.RFC3339, tc.now)
		if err != nil {
			t.Fatal(err)
		}
		if got, err := DueDate(tc.deadline, now.In(warsaw)); err != nil || got != tc.want {
			t.Errorf("%s/%s: got %q want %q: %v", tc.deadline, tc.now, got, tc.want, err)
		}
	}
}

type memoryState struct {
	sync.Persistence // Tests inject a failing source after staging.
	i                cloudstate.Intent
	failCommit       bool
}

func (s *memoryState) Work(ctx context.Context, f func(cloudstate.Intent) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.i.Status == cloudstate.Created || s.i.Status == cloudstate.Skipped {
		return nil
	}
	return f(s.i)
}
func (s *memoryState) Claim(_ context.Context, _ string) (cloudstate.Intent, bool, error) {
	insert := s.i.Status == cloudstate.Pending
	s.i.Status = cloudstate.Sending
	return s.i, insert, nil
}
func (s *memoryState) Resolve(ctx context.Context, _, status, id, reason string) error {
	if ctx.Err() != nil || (status == cloudstate.Created && s.failCommit) {
		return errors.New("commit interrupted")
	}
	s.i.Status, s.i.TaskID, s.i.Reason = status, id, reason
	return nil
}

type fakeTasks struct {
	calls    int
	onInsert func(context.Context, tasks.Task) (string, bool, error)
	matches  map[string][]tasks.Task
}

func (f *fakeTasks) Insert(ctx context.Context, task tasks.Task) (string, bool, error) {
	f.calls++
	return f.onInsert(ctx, task)
}
func (f *fakeTasks) Marked(context.Context) (map[string][]tasks.Task, error) { return f.matches, nil }

func TestDispatchRecovery(t *testing.T) {
	for _, mode := range []string{"lost response", "crash after insert", "crash before insert", "multiple matches"} {
		t.Run(mode, func(t *testing.T) {
			s := &memoryState{i: cloudstate.Intent{ID: "intent", Status: cloudstate.Pending, Deadline: "2026-11-11", Reference: "ref"}}
			if mode == "crash before insert" || mode == "multiple matches" {
				s.i.Status = cloudstate.Sending
			}
			s.failCommit = mode == "crash after insert"
			f := &fakeTasks{matches: make(map[string][]tasks.Task)}
			if mode == "multiple matches" {
				f.matches[tasks.Marker("intent")] = []tasks.Task{{ID: "first"}, {ID: "second"}}
			}
			f.onInsert = func(_ context.Context, task tasks.Task) (string, bool, error) {
				if task.Due != "2026-11-10T00:00:00Z" || task.Notes != "Payment deadline: 2026-11-11\n"+tasks.Marker("intent") || s.i.Status != cloudstate.Sending {
					t.Fatal(task, s.i)
				}
				f.matches[tasks.Marker("intent")] = []tasks.Task{{ID: "task-ID", Status: "completed"}}
				if mode == "lost response" {
					return "", true, context.DeadlineExceeded
				}
				return "task-ID", false, nil
			}
			now := func() time.Time { return time.Date(2026, 11, 9, 9, 0, 0, 0, time.UTC) }
			invoke := func() Result {
				return Run(t.Context(), s, func(context.Context) (sync.Downloader, error) { return nil, errors.New("partial KSeF result") }, f, now(), time.Minute, time.UTC, now)
			}
			first := invoke()
			if !first.DiscoveryError || first.Unresolved != 1 {
				t.Fatal(first)
			}
			s.failCommit = false
			second := invoke()
			if mode == "crash before insert" || mode == "multiple matches" {
				if second.Unresolved != 1 || f.calls != 0 || s.i.Status != cloudstate.Unresolved {
					t.Fatal(second, f.calls, s.i)
				}
				return
			}
			if second.Recovered != 1 || second.PaidRecovered != 1 || second.Unresolved != 0 || s.i.TaskID != "task-ID" {
				t.Fatal(second, s.i)
			}
			// Deletion is not payment; even with no list match the recorded ID is terminal.
			clear(f.matches)
			if third := invoke(); third.PaidRecovered != 0 || third.Recovered != 0 || f.calls != 1 {
				t.Fatal(third, f.calls)
			}
		})
	}
}

func TestDispatchAuthorizationAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	s := &memoryState{i: cloudstate.Intent{ID: "intent", Status: cloudstate.Pending, Deadline: "2026-11-11"}}
	f := &fakeTasks{onInsert: func(context.Context, tasks.Task) (string, bool, error) { return "", false, tasks.ErrAuthorization }}
	now := func() time.Time { return time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC) }
	invoke := func() Result {
		return Run(ctx, s, func(context.Context) (sync.Downloader, error) { return nil, ksef.ErrAuthorization }, f, now(), time.Minute, time.UTC, now)
	}
	if r := invoke(); !r.AuthorizationError || !r.DispatchError || s.i.Status != cloudstate.Pending {
		t.Fatal(r, s.i)
	}
	f.onInsert = func(ctx context.Context, task tasks.Task) (string, bool, error) {
		if task.Due != "2026-11-11T00:00:00Z" {
			t.Fatal(task)
		}
		cancel()
		return "", true, ctx.Err()
	}
	if r := invoke(); !r.DispatchError || r.Unresolved != 1 || s.i.Status != cloudstate.Sending {
		t.Fatal(r, s.i)
	}
	if r := invoke(); !r.DispatchError || f.calls != 2 {
		t.Fatal(r, f.calls)
	}
}
