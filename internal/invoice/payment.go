// Package invoice validates downloaded invoices and extracts explicit payment dates.
package invoice

import (
	"bytes"
	"context"
	"encoding/xml"
	"io"
	"strings"
	"time"
)

// These are the published FA(2) and FA(3) namespaces. Unknown schemas are skipped.
const fa2 = "http://crd.gov.pl/wzor/2023/06/29/12648/"
const fa3 = "http://crd.gov.pl/wzor/2025/06/25/13775/"

type document struct {
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

// PaymentDate expects a validated document. It does not infer dates or select installments.
func PaymentDate(ctx context.Context, b []byte) (time.Time, string, bool, error) {
	b = bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})
	// Reject foreign-namespace lookalikes rather than matching by local name.
	d := xml.NewDecoder(bytes.NewReader(b))
	namespace := ""
	for {
		if err := ctx.Err(); err != nil {
			return time.Time{}, "", false, err
		}
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
	var in document
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
	return date, number, true, ctx.Err()
}
