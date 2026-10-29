package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rkrokosz/pylon/internal/config"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
)

func TestExportICSOffline(t *testing.T) {
	t.Setenv("PYLON_KSEF_TOKEN", "")
	root := t.TempDir()
	s, err := storage.Open(root, "test", "5260250995")
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveXML("5260250995-20261001-ABCDEFABCDEF-AB", []byte(`<Faktura xmlns="http://crd.gov.pl/wzor/2025/06/25/13775/"><Fa><P_2>FV/1</P_2><Platnosc><TerminPlatnosci><Termin>2026-11-01</Termin></TerminPlatnosci></Platnosc></Fa></Faktura>`), storage.Metadata{Subject: "received", PermanentStorageDate: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	factory := func(config.Config) *ksef.Client { t.Fatal("export constructed a network client"); return nil }
	args := []string{"export-ics", "--nip", "5260250995", "--storage-dir", root}
	var out bytes.Buffer
	if err := runCLI(t.Context(), args, &out, factory); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "BEGIN:VCALENDAR\r\n") || strings.Count(out.String(), "BEGIN:VEVENT") != 1 || !strings.HasSuffix(out.String(), "END:VCALENDAR\r\n") {
		t.Fatal(out.String())
	}
	if err := runCLI(t.Context(), args, brokenWriter{}, factory); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("writer error lost", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	out.Reset()
	if err := runCLI(ctx, args, &out, factory); err == nil || out.Len() != 0 {
		t.Fatal("cancelled export wrote data", err)
	}
	for _, flag := range []string{"--from=2026-01-01T00:00:00Z", "--to=2027-01-01T00:00:00Z", "--token-file=missing"} {
		if err := runCLI(t.Context(), append(args, flag), &out, factory); err == nil {
			t.Fatal("sync flag accepted", flag)
		}
	}
}

type brokenWriter struct{}

func (brokenWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
