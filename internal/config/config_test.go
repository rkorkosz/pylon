package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseArgs(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		want    Options
		wantErr bool
	}{
		{name: "global help", args: []string{"--help"}, want: Options{Help: true}},
		{name: "short help", args: []string{"-h"}, want: Options{Help: true}},
		{name: "command help", args: []string{"sync", "--nip=5260250995", "--help"}, want: Options{Help: true, Nip: "5260250995"}},
		{name: "all options", args: []string{"sync", "--environment", "demo", "--nip=5260250995", "--subject", "both", "--from", "2025-01-01T00:00:00Z", "--to", "2025-02-01T00:00:00Z", "--storage-dir", "private", "--token-file", "token"}, want: Options{Environment: "demo", Nip: "5260250995", Subject: "both", From: "2025-01-01T00:00:00Z", To: "2025-02-01T00:00:00Z", StorageDir: "private", TokenFile: "token"}},
		{name: "flags before command", args: []string{"--nip", "5260250995", "sync", "--subject", "issued"}, want: Options{Nip: "5260250995", Subject: "issued"}},
		{name: "single dash", args: []string{"sync", "-nip", "5260250995"}, want: Options{Nip: "5260250995"}},
		{name: "repeated flag", args: []string{"sync", "--subject", "received", "--subject=issued"}, want: Options{Subject: "issued"}},
		{name: "unknown flag", args: []string{"sync", "--wat"}, wantErr: true},
		{name: "missing value", args: []string{"sync", "--nip"}, wantErr: true},
		{name: "missing command", wantErr: true},
		{name: "unknown command", args: []string{"other"}, wantErr: true},
		{name: "extra argument", args: []string{"sync", "extra"}, wantErr: true},
		{name: "help with invalid flag", args: []string{"sync", "--help", "--wat"}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseArgs(tt.args)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseArgs() error = %v, wantErr %v", err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Fatalf("ParseArgs() = %#v, want %#v", got, tt.want)
			}
		})
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
