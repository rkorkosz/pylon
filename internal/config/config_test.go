package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgs(t *testing.T) {
	o, e := ParseArgs([]string{"sync", "--nip=5260250995", "--help"})
	if e != nil || !o.Help {
		t.Fatalf("got %#v, %v", o, e)
	}
	if _, e = ParseArgs([]string{"sync", "--wat"}); e == nil {
		t.Fatal("unknown flag accepted")
	}
}
func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		o       Options
		wantErr bool
		env     string
	}{
		{"defaults", Options{Nip: "5260250995"}, false, "test"},
		{"bad NIP", Options{Nip: "123"}, true, ""},
		{"bad env", Options{Nip: "5260250995", Environment: "local"}, true, ""},
		{"bad subject", Options{Nip: "5260250995", Subject: "unknown"}, true, ""},
		{"bad date", Options{Nip: "5260250995", From: "yesterday"}, true, ""},
		{"inverted", Options{Nip: "5260250995", From: "2025-01-02T00:00:00Z", To: "2025-01-01T00:00:00Z"}, true, ""},
		{"production explicit", Options{Nip: "5260250995", Environment: "prod"}, false, "prod"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, e := Validate(tt.o, func(k string) string { return "token" })
			if (e != nil) != tt.wantErr {
				t.Fatalf("cfg %#v error %v", c, e)
			}
			if e == nil && c.Environment != tt.env {
				t.Fatalf("environment %q", c.Environment)
			}
		})
	}
}
func TestTokenFileMustBePrivate(t *testing.T) {
	d := t.TempDir()
	p := filepath.Join(d, "token")
	if e := os.WriteFile(p, []byte("secret"), 0600); e != nil {
		t.Fatal(e)
	}
	_, e := Validate(Options{Nip: "5260250995", TokenFile: p}, func(string) string { return "" })
	if e != nil {
		t.Fatal(e)
	}
	if e = os.Chmod(p, 0644); e != nil {
		t.Fatal(e)
	}
	if _, e = Validate(Options{Nip: "5260250995", TokenFile: p}, func(string) string { return "" }); e == nil {
		t.Fatal("permissive token file accepted")
	}
}
