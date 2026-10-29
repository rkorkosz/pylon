package calendar

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/rkrokosz/pylon/internal/storage"
)

func document(ns, terms string) []byte {
	return []byte(`<Faktura xmlns="` + ns + `"><Fa><P_2>FV/1</P_2><Platnosc>` + terms + `</Platnosc></Fa></Faktura>`)
}

func TestDueDate(t *testing.T) {
	explicit := `<TerminPlatnosci><Termin>2028-02-29</Termin></TerminPlatnosci>`
	for _, ns := range []string{fa2, fa3} {
		t.Run(ns, func(t *testing.T) {
			for _, tc := range []struct {
				name, terms string
				want        bool
			}{
				{"explicit", explicit, true},
				{"missing", "", false},
				{"empty", `<TerminPlatnosci/>`, false},
				{"description", `<TerminPlatnosci><TerminOpis>14 dni</TerminOpis></TerminPlatnosci>`, false},
				{"both", `<TerminPlatnosci><Termin>2028-02-29</Termin><TerminOpis/></TerminPlatnosci>`, false},
				{"multiple", explicit + explicit, false},
				{"duplicate dates", `<TerminPlatnosci><Termin>2028-02-29</Termin><Termin>2028-02-29</Termin></TerminPlatnosci>`, false},
				{"invalid", `<TerminPlatnosci><Termin>2027-02-29</Termin></TerminPlatnosci>`, false},
				{"timestamp", `<TerminPlatnosci><Termin>2028-02-29T00:00:00Z</Termin></TerminPlatnosci>`, false},
				{"wrong path", `<Other>` + explicit + `</Other>`, false},
				{"foreign namespace", `<TerminPlatnosci xmlns="urn:other"><Termin>2028-02-29</Termin></TerminPlatnosci>`, false},
			} {
				t.Run(tc.name, func(t *testing.T) {
					due, number, ok, err := dueDate(document(ns, tc.terms))
					if err != nil || ok != tc.want {
						t.Fatalf("ok=%v err=%v", ok, err)
					}
					if ok && (due.Format(time.DateOnly) != "2028-02-29" || number != "FV/1") {
						t.Fatal(due, number)
					}
				})
			}
		})
	}
	for _, b := range [][]byte{document("urn:unknown", explicit), []byte(`<Faktura/>`), []byte(`<Faktura xmlns="` + fa3 + `"><Fa/><Fa/></Faktura>`)} {
		if _, _, ok, err := dueDate(b); ok || err != nil {
			t.Fatal(ok, err)
		}
	}
	if _, _, _, err := dueDate([]byte(`<Faktura xmlns="` + fa3 + `">`)); err == nil {
		t.Fatal("malformed XML accepted")
	}
}

func TestExport(t *testing.T) {
	root := t.TempDir()
	store, err := storage.Open(root, "test", "5260250995")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	}()
	number := "5260250995-20261001-ABCDEFABCDEF-AB"
	stamp := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	m := storage.Metadata{Subject: "received", PermanentStorageDate: stamp, DownloadedAt: stamp}
	b := document(fa3, `<TerminPlatnosci><Termin>2028-12-31</Termin></TerminPlatnosci>`)
	if _, err := store.SaveXML(number, b, m); err != nil {
		t.Fatal(err)
	}
	first, err := Export(t.Context(), store, "test", "5260250995", "received")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(first), "\r\n ", "")
	for _, want := range []string{"BEGIN:VCALENDAR\r\n", "DTSTART;VALUE=DATE:20281231\r\n", "DTEND;VALUE=DATE:20290101\r\n", "DTSTAMP:20261001T120000Z\r\n", "SUMMARY:Invoice due: FV/1\r\n", "CLASS:PRIVATE\r\n", "END:VCALENDAR\r\n"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q in %s", want, text)
		}
	}
	again, err := Export(t.Context(), store, "test", "5260250995", "both")
	if err != nil || string(again) != string(first) {
		t.Fatal("not stable", err)
	}
	issued, err := Export(t.Context(), store, "test", "5260250995", "issued")
	if err != nil || strings.Contains(string(issued), "VEVENT") {
		t.Fatal("subject filter", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := Export(ctx, store, "test", "5260250995", "both"); err == nil {
		t.Fatal("cancellation ignored")
	}
	if err := os.WriteFile(filepath.Join(root, "test", "5260250995", "invoices", number+".xml"), []byte(`<Faktura/>`), 0600); err != nil {
		t.Fatal(err)
	}
	if b, err := Export(t.Context(), store, "test", "5260250995", "both"); err == nil || b != nil {
		t.Fatal("corruption produced output", err)
	}
}

func TestTextAndFolding(t *testing.T) {
	if got := escapeText("a\\b,c;d\r\nBEGIN:VEVENT\ne"); got != `a\\b\,c\;d\nBEGIN:VEVENT\ne` {
		t.Fatal(got)
	}
	original := "SUMMARY:" + strings.Repeat("Zażółć", 40)
	var out strings.Builder
	foldLine(&out, original)
	for _, line := range strings.Split(strings.TrimSuffix(out.String(), "\r\n"), "\r\n") {
		if len(line) > 75 || !utf8.ValidString(line) {
			t.Fatal("invalid folded line", line)
		}
	}
	if strings.ReplaceAll(strings.TrimSuffix(out.String(), "\r\n"), "\r\n ", "") != original {
		t.Fatal("folding lost text")
	}
}
