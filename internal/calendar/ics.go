// Package calendar exports explicit KSeF payment deadlines as RFC 5545 events.
package calendar

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/rkrokosz/pylon/internal/invoice"
	"github.com/rkrokosz/pylon/internal/storage"
)

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
		due, number, ok, err := invoice.PaymentDate(ctx, b)
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
