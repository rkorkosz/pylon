package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func number(i int) string { return fmt.Sprintf("5260250995-20250101-%012X-00", i) }
func metadata() Metadata {
	return Metadata{PermanentStorageDate: time.Date(2025, 1, 1, 1, 0, 0, 0, time.UTC), Subject: "received"}
}
func testStore(t *testing.T) *Store {
	t.Helper()
	s, e := Open(t.TempDir(), "test", "5260250995")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func TestSaveXMLIdempotentAndConflict(t *testing.T) {
	s := testStore(t)
	data := []byte("<?xml version=\"1.0\"?>\n<Invoice a=\"&amp;\"/>\n")
	n := number(1)
	m := metadata()
	sum := sha256.Sum256(data)
	m.APIHash = base64.StdEncoding.EncodeToString(sum[:])
	for i := 0; i < 2; i++ {
		created, e := s.SaveXML(n, data, m)
		if e != nil || created != (i == 0) {
			t.Fatalf("save %d: %v %v", i, created, e)
		}
	}
	got, e := os.ReadFile(filepath.Join(s.dir, "invoices", n+".xml"))
	if e != nil || !bytes.Equal(data, got) {
		t.Fatal("XML bytes changed", e)
	}
	if ok, e := s.ExistingValid(n); e != nil || !ok {
		t.Fatal(ok, e)
	}
	if _, e = s.SaveXML(n, []byte(`<Invoice>different</Invoice>`), metadata()); e == nil {
		t.Fatal("conflict overwritten")
	}
	bad := m
	bad.APIHash = "incorrect"
	if _, e = s.SaveXML(number(2), data, bad); e == nil {
		t.Fatal("bad API hash accepted")
	}
	for _, name := range []string{n + ".xml", n + ".json"} {
		fi, e := os.Stat(filepath.Join(s.dir, "invoices", name))
		if e != nil || fi.Mode().Perm() != 0600 {
			t.Fatal("file not private", e)
		}
	}
}
func TestRejectBadXMLAndPath(t *testing.T) {
	s := testStore(t)
	for _, n := range []string{"../evil", "", "A-1", strings.ToLower(number(10)), "0000000000-20250101-000000000001-00"} {
		if _, e := s.SaveXML(n, []byte(`<Invoice/>`), metadata()); e == nil {
			t.Fatalf("accepted number %q", n)
		}
	}
	for _, b := range []string{"", `<Invoice>`, `<Invoice/><Other/>`, `text<Invoice/>`, `<Invoice/>trailing`, `<Invoice/><?xml version="1.0"?>`, `<html/>`, `<!DOCTYPE Invoice><Invoice/>`, `{"error":"no"}`} {
		t.Run(b, func(t *testing.T) {
			if _, e := s.SaveXML(number(1), []byte(b), metadata()); e == nil {
				t.Fatal("invalid body accepted")
			}
		})
	}
	if _, e := s.SaveXML(number(1), bytes.Repeat([]byte("x"), maxXML+1), metadata()); e == nil {
		t.Fatal("oversized body accepted")
	}
	legacy := "5260250995-20250101-000000-000001-00"
	if _, e := s.SaveXML(legacy, []byte(`<Invoice/>`), metadata()); e != nil {
		t.Fatal("legacy identifier rejected", e)
	}
}
func TestBOMPreservation(t *testing.T) {
	s := testStore(t)
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(`<?xml version="1.0"?><Invoice/>`)...)
	if _, e := s.SaveXML(number(1), data, metadata()); e != nil {
		t.Fatal(e)
	}
	got, e := os.ReadFile(filepath.Join(s.dir, "invoices", number(1)+".xml"))
	if e != nil || !bytes.Equal(got, data) {
		t.Fatal("BOM changed", e)
	}
}

func TestDirectoryPermissionFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	s := testStore(t)
	path := filepath.Join(s.dir, "invoices")
	if e := os.Chmod(path, 0500); e != nil {
		t.Fatal(e)
	}
	defer func() {
		if err := os.Chmod(path, 0700); err != nil {
			t.Error(err)
		}
	}()
	if _, e := s.SaveXML(number(1), []byte(`<Invoice/>`), metadata()); e == nil {
		t.Fatal("permission failure ignored")
	}
}

func TestInterruptedPairsAndCorruption(t *testing.T) {
	for _, missing := range []string{"xml", "json"} {
		t.Run(missing, func(t *testing.T) {
			s := testStore(t)
			n := number(1)
			b := []byte(`<Invoice/>`)
			if _, e := s.SaveXML(n, b, metadata()); e != nil {
				t.Fatal(e)
			}
			if e := os.Remove(filepath.Join(s.dir, "invoices", n+"."+missing)); e != nil {
				t.Fatal(e)
			}
			if ok, e := s.ExistingValid(n); e != nil || ok {
				t.Fatal("incomplete pair valid", ok, e)
			}
			if _, e := s.SaveXML(n, b, metadata()); e != nil {
				t.Fatal("pair not recovered", e)
			}
			if ok, e := s.ExistingValid(n); e != nil || !ok {
				t.Fatal(ok, e)
			}
		})
	}
	for _, corrupt := range []string{"xml", "json", "identity", "date", "apiHash"} {
		t.Run(corrupt, func(t *testing.T) {
			s := testStore(t)
			n := number(1)
			if _, e := s.SaveXML(n, []byte(`<Invoice/>`), metadata()); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(s.dir, "invoices", n+".json")
			var b []byte
			switch corrupt {
			case "xml":
				path = filepath.Join(s.dir, "invoices", n+".xml")
				b = []byte(`<Invoice>changed</Invoice>`)
			case "json":
				b = []byte(`{`)
			default:
				raw, e := os.ReadFile(path)
				if e != nil {
					t.Fatal(e)
				}
				var m Metadata
				if e = json.Unmarshal(raw, &m); e != nil {
					t.Fatal(e)
				}
				switch corrupt {
				case "identity":
					m.KSeFNumber = number(2)
				case "date":
					m.PermanentStorageDate = time.Time{}
				case "apiHash":
					m.APIHash = "bad"
				}
				b, _ = json.Marshal(m)
			}
			if e := os.WriteFile(path, b, 0600); e != nil {
				t.Fatal(e)
			}
			if _, e := s.ExistingValid(n); e == nil {
				t.Fatal("corruption accepted")
			}
		})
	}
}
func TestSubjectProvenanceAndLegacySidecar(t *testing.T) {
	s := testStore(t)
	n := number(1)
	b := []byte(`<Invoice/>`)
	m := metadata()
	if _, e := s.SaveXML(n, b, m); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(s.dir, "invoices", n+".json")
	raw, _ := os.ReadFile(path)
	var old Metadata
	if err := json.Unmarshal(raw, &old); err != nil {
		t.Fatal(err)
	}
	old.Subjects = nil
	raw, _ = json.Marshal(old)
	if e := os.WriteFile(path, raw, 0600); e != nil {
		t.Fatal(e)
	}
	issued := m
	issued.Subject = "issued"
	if ok, e := s.ExistingValid(n, issued); e != nil || !ok {
		t.Fatal(ok, e)
	}
	raw, _ = os.ReadFile(path)
	var got Metadata
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Subjects, []string{"issued", "received"}) || got.Subject != "received" || !got.DownloadedAt.Equal(old.DownloadedAt) {
		t.Fatalf("metadata %#v", got)
	}
	issued.PermanentStorageDate = issued.PermanentStorageDate.Add(time.Second)
	if _, e := s.ExistingValid(n, issued); e == nil {
		t.Fatal("conflicting storage date accepted")
	}
	issued = m
	issued.APIHash = "bad"
	if _, e := s.ExistingValid(n, issued); e == nil {
		t.Fatal("new conflicting API hash accepted")
	}
}
func TestRejectArtifactSymlinksAndPermissions(t *testing.T) {
	for _, artifact := range []string{"xml", "json", "checkpoint", "lock"} {
		t.Run(artifact, func(t *testing.T) {
			root := t.TempDir()
			s, e := Open(root, "test", "5260250995")
			if e != nil {
				t.Fatal(e)
			}
			n := number(1)
			external := filepath.Join(t.TempDir(), "external")
			body := []byte(`<Invoice/>`)
			if e = os.WriteFile(external, body, 0600); e != nil {
				t.Fatal(e)
			}
			path := filepath.Join(s.dir, "invoices", n+"."+artifact)
			if artifact == "checkpoint" {
				path = filepath.Join(s.dir, "checkpoints", "received.json")
			}
			if artifact == "lock" {
				path = filepath.Join(s.dir, ".lock")
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if e = os.Remove(path); e != nil {
					t.Fatal(e)
				}
			} else {
				defer func() {
					if err := s.Close(); err != nil {
						t.Error(err)
					}
				}()
			}
			if e = os.Symlink(external, path); e != nil {
				t.Fatal(e)
			}
			switch artifact {
			case "lock":
				if other, e := Open(root, "test", "5260250995"); e == nil {
					if err := other.Close(); err != nil {
						t.Error(err)
					}
					t.Fatal("lock symlink accepted")
				}
			case "checkpoint":
				if _, _, e = s.Checkpoint("received"); e == nil {
					t.Fatal("checkpoint symlink read")
				}
				if e = s.SaveCheckpoint(checkpoint()); e == nil {
					t.Fatal("checkpoint symlink replaced")
				}
			default:
				if _, e = s.SaveXML(n, body, metadata()); e == nil {
					t.Fatal("artifact symlink accepted")
				}
				if _, e = s.ExistingValid(n); e == nil {
					t.Fatal("artifact symlink read")
				}
			}
			got, _ := os.ReadFile(external)
			if !bytes.Equal(got, body) {
				t.Fatal("external file modified")
			}
		})
	}
	s := testStore(t)
	n := number(1)
	if _, e := s.SaveXML(n, []byte(`<Invoice/>`), metadata()); e != nil {
		t.Fatal(e)
	}
	if e := os.Chmod(filepath.Join(s.dir, "invoices", n+".xml"), 0644); e != nil {
		t.Fatal(e)
	}
	if _, e := s.ExistingValid(n); e == nil {
		t.Fatal("nonprivate artifact accepted")
	}
}
func TestRejectDirectorySymlinks(t *testing.T) {
	for _, component := range []string{"root", "environment", "namespace", "invoices", "checkpoints"} {
		t.Run(component, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "data")
			external := t.TempDir()
			if e := os.Chmod(external, 0755); e != nil {
				t.Fatal(e)
			}
			p := root
			switch component {
			case "environment":
				p = filepath.Join(root, "test")
			case "namespace":
				p = filepath.Join(root, "test", "5260250995")
			case "invoices", "checkpoints":
				p = filepath.Join(root, "test", "5260250995", component)
			}
			if e := os.MkdirAll(filepath.Dir(p), 0700); e != nil {
				t.Fatal(e)
			}
			if e := os.Symlink(external, p); e != nil {
				t.Fatal(e)
			}
			if s, e := Open(root, "test", "5260250995"); e == nil {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
				t.Fatal("directory symlink accepted")
			}
			fi, _ := os.Stat(external)
			if fi.Mode().Perm() != 0755 {
				t.Fatal("symlink target chmodded")
			}
		})
	}
}
func checkpoint() Checkpoint {
	return Checkpoint{Environment: "test", NIP: "5260250995", Subject: "received", Cursor: metadata().PermanentStorageDate}
}
func TestCheckpointNamespacesAndLock(t *testing.T) {
	root := t.TempDir()
	s, e := Open(root, "test", "5260250995")
	if e != nil {
		t.Fatal(e)
	}
	if other, e := Open(root, "test", "5260250995"); e == nil {
		if err := other.Close(); err != nil {
			t.Error(err)
		}
		t.Fatal("second writer acquired lock")
	}
	cp := checkpoint()
	if e = s.SaveCheckpoint(cp); e != nil {
		t.Fatal(e)
	}
	got, ok, e := s.Checkpoint("received")
	if e != nil || !ok || !got.Cursor.Equal(cp.Cursor) {
		t.Fatal(got, ok, e)
	}
	cp.NIP = "1111111111"
	if e = s.SaveCheckpoint(cp); e == nil {
		t.Fatal("wrong namespace saved")
	}
	if _, _, e = s.Checkpoint("../received"); e == nil {
		t.Fatal("subject traversal accepted")
	}
	if e = s.Close(); e != nil {
		t.Fatal(e)
	}
	s, e = Open(root, "test", "5260250995")
	if e != nil {
		t.Fatal("lock not released", e)
	}
	defer func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	}()
}
func TestCancellationAndFailedCommit(t *testing.T) {
	s := testStore(t)
	n := number(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := s.SaveXMLContext(ctx, n, []byte(`<Invoice/>`), metadata()); e != context.Canceled {
		t.Fatal(e)
	}
	if _, e := os.Stat(filepath.Join(s.dir, "invoices", n+".xml")); !os.IsNotExist(e) {
		t.Fatal("cancelled write committed")
	}
	// A metadata commit error leaves no pair reported complete.
	if e := os.Mkdir(filepath.Join(s.dir, "invoices", n+".json"), 0700); e != nil {
		t.Fatal(e)
	}
	if _, e := s.SaveXML(n, []byte(`<Invoice/>`), metadata()); e == nil {
		t.Fatal("special metadata path accepted")
	}
	if ok, _ := s.ExistingValid(n); ok {
		t.Fatal("failed write reported complete")
	}
	temps, _ := filepath.Glob(filepath.Join(s.dir, "invoices", ".pylon-tmp-*"))
	if len(temps) != 0 {
		t.Fatal("temporary writes leaked", temps)
	}
}
