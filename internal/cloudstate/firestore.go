package cloudstate

import (
	"context"
	"crypto/rand"
	"errors"
	"time"

	"cloud.google.com/go/firestore"
	"github.com/rkrokosz/pylon/internal/storage"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type namespace struct {
	Bootstrap string    `firestore:"bootstrap"`
	Cursor    string    `firestore:"cursor"`
	Owner     string    `firestore:"owner"`
	Expires   time.Time `firestore:"expires"`
}

// Run is a fenced lease. All mutations read its owner in the same transaction.
type Run struct {
	db    *firestore.Client
	root  *firestore.DocumentRef
	owner string
}

func Acquire(ctx context.Context, db *firestore.Client, deployment, env, nip, list string, start, expires time.Time) (*Run, error) {
	if deadline, ok := ctx.Deadline(); !ok || !expires.After(deadline) || start.IsZero() {
		return nil, errors.New("lease must outlive the work deadline")
	}
	r := &Run{db: db, root: db.Collection("pylonRuns").Doc(key(deployment, env, nip, list)), owner: rand.Text()}
	err := db.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		var ns namespace
		ok, err := read(tx.Get, r.root, &ns)
		if err != nil {
			return err
		}
		if ok && ns.Owner != "" && ns.Expires.After(time.Now()) {
			return ErrLease
		}
		if !ok {
			ns.Bootstrap = start.UTC().Format(time.RFC3339Nano)
			ns.Cursor = ns.Bootstrap
		}
		ns.Owner, ns.Expires = r.owner, expires
		return tx.Set(r.root, ns)
	})
	if err != nil {
		return nil, err
	}
	return r, nil
}

func read(get func(*firestore.DocumentRef) (*firestore.DocumentSnapshot, error), ref *firestore.DocumentRef, out any) (bool, error) {
	doc, err := get(ref)
	if status.Code(err) == codes.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, doc.DataTo(out)
}

func (r *Run) owned(ctx context.Context, f func(*firestore.Transaction, namespace) error) error {
	return r.db.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		var ns namespace
		ok, err := read(tx.Get, r.root, &ns)
		if err != nil {
			return err
		}
		if !ok || ns.Owner != r.owner || !ns.Expires.After(time.Now()) {
			return ErrLease
		}
		return f(tx, ns)
	})
}

func (r *Run) Release(ctx context.Context) error {
	return r.owned(ctx, func(tx *firestore.Transaction, ns namespace) error {
		ns.Owner = ""
		return tx.Set(r.root, ns)
	})
}

func (r *Run) Cursor(ctx context.Context, subject string) (t time.Time, ok bool, err error) {
	if subject != "received" {
		return t, false, errors.New("cloud cursors are received-only")
	}
	var ns namespace
	ok, err = read(func(ref *firestore.DocumentRef) (*firestore.DocumentSnapshot, error) { return ref.Get(ctx) }, r.root, &ns)
	if err != nil {
		return t, false, err
	}
	if !ok || ns.Owner != r.owner || !ns.Expires.After(time.Now()) {
		return t, false, ErrLease
	}
	t, err = time.Parse(time.RFC3339Nano, ns.Cursor)
	return t, err == nil, err
}

func (r *Run) SaveCursor(ctx context.Context, subject string, cursor time.Time) error {
	if subject != "received" {
		return errors.New("cloud cursors are received-only")
	}
	return r.owned(ctx, func(tx *firestore.Transaction, ns namespace) error {
		old, err := time.Parse(time.RFC3339Nano, ns.Cursor)
		if err != nil {
			return err
		}
		if !cursor.After(old) {
			return errors.New("non-advancing cursor")
		}
		ns.Cursor = cursor.UTC().Format(time.RFC3339Nano)
		return tx.Set(r.root, ns)
	})
}

func (r *Run) intents() *firestore.CollectionRef       { return r.root.Collection("intents") }
func (r *Run) invoice(n string) *firestore.DocumentRef { return r.intents().Doc(key(r.root.ID, n)) }

func (r *Run) Existing(ctx context.Context, n string, m storage.Metadata) (bool, error) {
	if err := checkMetadata(n, m); err != nil {
		return false, err
	}
	var i Intent
	ok, err := read(func(ref *firestore.DocumentRef) (*firestore.DocumentSnapshot, error) { return ref.Get(ctx) }, r.invoice(n), &i)
	if err != nil || !ok {
		return false, err
	}
	return true, i.verify(m)
}

func (r *Run) Save(ctx context.Context, n string, b []byte, m storage.Metadata) (bool, error) {
	i, err := Prepare(ctx, n, b, m)
	if err != nil {
		return false, err
	}
	ref := r.invoice(n)
	err = r.owned(ctx, func(tx *firestore.Transaction, _ namespace) error {
		var existing Intent
		ok, err := read(tx.Get, ref, &existing)
		if err != nil {
			return err
		}
		if ok {
			return existing.verify(m)
		}
		return tx.Create(ref, i)
	})
	return i.Status == Skipped, err
}

// Work visits a query snapshot; terminal intents are not scanned on later runs.
func (r *Run) Work(ctx context.Context, visit func(Intent) error) error {
	iter := r.intents().Where("status", "in", []string{Pending, Sending, Unresolved}).Documents(ctx)
	defer iter.Stop()
	for {
		doc, err := iter.Next()
		if errors.Is(err, iterator.Done) {
			return nil
		}
		if err != nil {
			return err
		}
		var i Intent
		if err := doc.DataTo(&i); err != nil {
			return err
		}
		i.ID = doc.Ref.ID
		if err := visit(i); err != nil {
			return err
		}
	}
}

// Claim persists the pre-send state. Only a fresh pending intent may be inserted.
func (r *Run) Claim(ctx context.Context, id string) (i Intent, insert bool, err error) {
	ref := r.intents().Doc(id)
	err = r.owned(ctx, func(tx *firestore.Transaction, _ namespace) error {
		i = Intent{ID: id}
		ok, err := read(tx.Get, ref, &i)
		if err != nil {
			return err
		}
		if !ok || i.TaskID != "" || (i.Status != Pending && i.Status != Sending && i.Status != Unresolved) || i.Claim == r.owner {
			return errors.New("intent cannot be claimed")
		}
		insert = i.Status == Pending
		i.Status, i.Claim = Sending, r.owner
		return tx.Set(ref, i)
	})
	return i, insert, err
}

func (r *Run) Resolve(ctx context.Context, id, next, taskID, reason string) error {
	if (next == Created) != (taskID != "") || (next != Created && next != Skipped && next != Pending && next != Unresolved) {
		return errors.New("invalid dispatch outcome")
	}
	ref := r.intents().Doc(id)
	return r.owned(ctx, func(tx *firestore.Transaction, _ namespace) error {
		var i Intent
		ok, err := read(tx.Get, ref, &i)
		if err != nil {
			return err
		}
		if !ok || i.Status != Sending || i.Claim != r.owner || i.TaskID != "" {
			return errors.New("intent claim lost")
		}
		i.Status, i.TaskID, i.Reason, i.Claim = next, taskID, reason, ""
		return tx.Set(ref, i)
	})
}
