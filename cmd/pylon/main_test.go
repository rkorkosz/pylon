package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/rkrokosz/pylon/internal/config"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
)

func TestHelpNeedsNoCredentials(t *testing.T) {
	t.Setenv("PYLON_KSEF_TOKEN", "")
	if e := run([]string{"--help"}); e != nil {
		t.Fatal(e)
	}
}
func TestPreflightDoesNotConstructClient(t *testing.T) {
	t.Setenv("PYLON_KSEF_TOKEN", "synthetic")
	for _, mode := range []string{"missing start", "wrong checkpoint", "checkpoint symlink"} {
		t.Run(mode, func(t *testing.T) {
			root := t.TempDir()
			s, e := storage.Open(root, "test", "5260250995")
			if e != nil {
				t.Fatal(e)
			}
			s.Close()
			path := filepath.Join(root, "test", "5260250995", "checkpoints", "received.json")
			switch mode {
			case "wrong checkpoint":
				if e = os.WriteFile(path, []byte(`{"version":99}`), 0600); e != nil {
					t.Fatal(e)
				}
			case "checkpoint symlink":
				if e = os.Symlink(filepath.Join(t.TempDir(), "absent"), path); e != nil {
					t.Fatal(e)
				}
			}
			called := false
			err := runCLI(context.Background(), []string{"sync", "--nip", "5260250995", "--storage-dir", root}, &bytes.Buffer{}, func(c config.Config) *ksef.Client { called = true; return nil })
			if err == nil || called {
				t.Fatal("preflight reached network", err, called)
			}
		})
	}
}

func TestValidationBeforeNetworkAndCredentials(t *testing.T) {
	t.Setenv("PYLON_KSEF_TOKEN", "")
	if e := run([]string{"sync", "--nip", "bad"}); e == nil {
		t.Fatal("invalid NIP accepted")
	}
	if e := run([]string{"sync", "--nip", "5260250995", "--from", "2025-01-01T00:00:00Z", "--storage-dir", t.TempDir()}); e == nil {
		t.Fatal("missing token accepted")
	}
	if _, e := os.Stat("data"); e == nil {
		t.Fatal("unexpected default data directory created")
	}
}
