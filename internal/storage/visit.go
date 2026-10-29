package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
)

// VisitInvoices reads validated XML/sidecar pairs in filename order under the
// namespace lock. Missing sidecars and orphan sidecars are errors, not omissions.
func (s *Store) VisitInvoices(ctx context.Context, subject string, visit func(Metadata, []byte) error) error {
	if subject != "both" && !validSubject(subject) {
		return errors.New("invalid invoice subject")
	}
	dir, err := openDir(s.invoices, ".", false)
	if err != nil {
		return err
	}
	defer func() { _ = dir.Close() }() // Read-only directory handle.
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".xml") && !strings.HasSuffix(name, ".json") {
			continue
		}
		number := strings.TrimSuffix(strings.TrimSuffix(name, ".xml"), ".json")
		xp, _, err := invoiceNames(number)
		if err != nil {
			return err
		}
		b, err := readFile(s.invoices, xp, maxXML)
		if err != nil {
			return err
		}
		m, exists, err := s.metadata(number, b)
		if err != nil {
			return err
		}
		if !exists {
			return fmt.Errorf("missing invoice metadata %s", number)
		}
		if strings.HasSuffix(name, ".json") {
			continue
		}
		if err := validateXML(ctx, b); err != nil {
			return err
		}
		if subject != "both" && m.Subject != subject && !slices.Contains(m.Subjects, subject) {
			continue
		}
		if err := visit(m, b); err != nil {
			return err
		}
	}
	return nil
}
