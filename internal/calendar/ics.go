// Package calendar exports explicit KSeF payment deadlines as RFC 5545 events.
package calendar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/xml"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rkrokosz/pylon/internal/storage"
)

// These are the published FA(2) and FA(3) namespaces. Unknown schemas are skipped.
const fa2 = "http://crd.gov.pl/wzor/2023/06/29/12648/"
const fa3 = "http://crd.gov.pl/wzor/2025/06/25/13775/"

type invoice struct {
	XMLName xml.Name `xml:"Faktura"`
	Fa      []struct {
		Numbers  []string `xml:"P_2"`
		Payments []struct {
			Terms []struct {
				Dates        []string   `xml:"Termin"`
				Descriptions []struct{} `xml:"TerminOpis"`
			} `xml:"TerminPlatnosci"`
		} `xml:"Platnosc"`
	} `xml:"Fa"`
}

// dueDate deliberately does not calculate dates from descriptions, select one
// installment, or infer dates from issue/sale dates. This is not XSD validation.
func dueDate(b []byte) (time.Time, string, bool, error) {
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	// Reject foreign-namespace lookalikes rather than letting encoding/xml match
	// them by local name. Storage already verifies document well-formedness.
	d := xml.NewDecoder(bytes.NewReader(b))
	namespace := ""
	for {
		token, err := d.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return time.Time{}, "", false, err
		}
		if start, ok := token.(xml.StartElement); ok {
			if namespace == "" {
				namespace = start.Name.Space
				if start.Name.Local != "Faktura" || (namespace != fa2 && namespace != fa3) {
					return time.Time{}, "", false, nil
				}
			}
			if start.Name.Space != namespace {
				return time.Time{}, "", false, nil
			}
		}
	}
	var in invoice
	if err := xml.Unmarshal(b, &in); err != nil {
		return time.Time{}, "", false, err
	}
	if len(in.Fa) != 1 || len(in.Fa[0].Payments) != 1 {
		return time.Time{}, "", false, nil
	}
	terms := in.Fa[0].Payments[0].Terms
	if len(terms) != 1 || len(terms[0].Dates) != 1 || len(terms[0].Descriptions) != 0 {
		return time.Time{}, "", false, nil
	}
	date, err := time.Parse(time.DateOnly, strings.TrimSpace(terms[0].Dates[0]))
	if err != nil || date.Year() < 1 || date.Year() >= 9999 {
		return time.Time{}, "", false, nil
	}
	number := ""
	if len(in.Fa[0].Numbers) == 1 {
		number = strings.TrimSpace(in.Fa[0].Numbers[0])
	}
	return date, number, true, nil
}

// Export buffers the calendar so corrupt storage never produces a partial
// calendar. UIDs include the namespace and invoice identity, not the due date.
// Paid invoices are not filtered: this is a deadline export, not a payment ledger.
func Export(ctx context.Context, store *storage.Store, environment, nip, subject string) ([]byte, error) {
	var out strings.Builder
	line := func(s string) { foldLine(&out, s) }
	line("BEGIN:VCALENDAR")
	line("VERSION:2.0")
	line("PRODID:-//pylon//Invoice due dates//EN")
	line("CALSCALE:GREGORIAN")
	err := store.VisitInvoices(ctx, subject, func(m storage.Metadata, b []byte) error {
		due, number, ok, err := dueDate(b)
		if err != nil {
			return fmt.Errorf("parse invoice %s: %w", m.KSeFNumber, err)
		}
		if !ok {
			return nil
		}
		if number == "" {
			number = m.KSeFNumber
		}
		uid := sha256.Sum256([]byte(environment + "/" + nip + "/" + m.KSeFNumber))
		line("BEGIN:VEVENT")
		line(fmt.Sprintf("UID:%x@pylon", uid))
		line("DTSTAMP:" + m.DownloadedAt.UTC().Format("20060102T150405Z"))
		line("DTSTART;VALUE=DATE:" + due.Format("20060102"))
		line("DTEND;VALUE=DATE:" + due.AddDate(0, 0, 1).Format("20060102"))
		line("SUMMARY:" + escapeText("Invoice due: "+number))
		line("DESCRIPTION:" + escapeText("KSeF: "+m.KSeFNumber+"\nEnvironment: "+environment+"\nPayment status is not tracked."))
		line("CLASS:PRIVATE")
		line("TRANSP:TRANSPARENT")
		line("END:VEVENT")
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	line("END:VCALENDAR")
	return []byte(out.String()), nil
}

func escapeText(s string) string {
	return strings.NewReplacer("\\", "\\\\", "\r\n", "\\n", "\r", "\\n", "\n", "\\n", ";", "\\;", ",", "\\,").Replace(s)
}

// RFC 5545 limits physical lines to 75 octets; never split a UTF-8 rune.
func foldLine(out *strings.Builder, s string) {
	width := 75
	for len(s) > width {
		n := width
		for !utf8.RuneStart(s[n]) {
			n--
		}
		out.WriteString(s[:n])
		out.WriteString("\r\n ")
		s = s[n:]
		width = 74 // The continuation space counts toward the limit.
	}
	out.WriteString(s)
	out.WriteString("\r\n")
}
