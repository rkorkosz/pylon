package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestVisitInvoices(t *testing.T) {
	const number = "5260250995-20261001-ABCDEFABCDEF-AB"
	for _, mode := range []string{"valid", "missing sidecar", "orphan sidecar", "symlink", "public"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			s, err := Open(root, "test", "5260250995")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			m := Metadata{Subject: "issued", PermanentStorageDate: time.Now()}
			if _, err := s.SaveXML(number, []byte(`<Faktura/>`), m); err != nil {
				t.Fatal(err)
			}
			// An invoice observed in both directions must still be visited only once.
			m.Subject = "received"
			if _, err := s.SaveXML(number, []byte(`<Faktura/>`), m); err != nil {
				t.Fatal(err)
			}
			base := filepath.Join(root, "test", "5260250995", "invoices", number)
			switch mode {
			case "missing sidecar":
				err = os.Remove(base + ".json")
			case "orphan sidecar":
				err = os.Remove(base + ".xml")
			case "symlink":
				if err = os.Remove(base + ".xml"); err == nil {
					err = os.Symlink("/dev/null", base+".xml")
				}
			case "public":
				err = os.Chmod(base+".xml", 0644)
			}
			if err != nil {
				t.Fatal(err)
			}
			for _, subject := range []string{"received", "issued", "both"} {
				count := 0
				err := s.VisitInvoices(t.Context(), subject, func(Metadata, []byte) error { count++; return nil })
				if mode == "valid" {
					if err != nil || count != 1 {
						t.Fatal(count, err)
					}
				} else if err == nil {
					t.Fatal("unsafe storage accepted")
				}
			}
		})
	}
}
