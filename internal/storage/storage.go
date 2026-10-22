package storage

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
)

// KsefNumber, TEST OpenAPI 2.9.0; includes the legacy 36-character form.
var numberPattern = regexp.MustCompile(`^([1-9]([0-9][1-9]|[1-9][0-9])[0-9]{7})-(20[2-9][0-9]|2[1-9][0-9]{2}|[3-9][0-9]{3})(0[1-9]|1[0-2])(0[1-9]|[12][0-9]|3[01])-([0-9A-F]{6})-?([0-9A-F]{6})-([0-9A-F]{2})$`)

const CheckpointVersion = 1
const maxXML = 50 << 20

type Metadata struct {
	KSeFNumber           string    `json:"ksefNumber"`
	PermanentStorageDate time.Time `json:"permanentStorageDate"`
	Subject              string    `json:"subject"` // First observed direction; retained for old sidecars.
	Subjects             []string  `json:"subjects,omitempty"`
	DownloadedAt         time.Time `json:"downloadedAt"`
	ByteLength           int64     `json:"byteLength"`
	SHA256               string    `json:"sha256"`
	APIHash              string    `json:"apiHash,omitempty"`
}
type Checkpoint struct {
	Version       int       `json:"version"`
	Environment   string    `json:"environment"`
	NIP           string    `json:"nip"`
	Subject       string    `json:"subject"`
	FilterVersion int       `json:"filterVersion"`
	Cursor        time.Time `json:"cursor"`
}
type Store struct {
	Root                        string
	dir                         string
	lock, invoices, checkpoints *os.File
}

// All artifact operations use pinned directory descriptors and single-component
// names. O_NOFOLLOW rejects links, including links to other namespaces.
func openDir(parent *os.File, name string, create bool) (*os.File, error) {
	if create {
		if e := syscall.Mkdirat(int(parent.Fd()), name, 0700); e != nil && !errors.Is(e, syscall.EEXIST) {
			return nil, e
		}
	}
	fd, e := syscall.Openat(int(parent.Fd()), name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if e != nil {
		return nil, fmt.Errorf("open storage directory %s (symlinks are not supported): %w", name, e)
	}
	f := os.NewFile(uintptr(fd), name)
	if create {
		if e = f.Chmod(0700); e == nil {
			e = parent.Sync()
		}
		if e != nil {
			_ = f.Close() // Preserve the original setup error.
			return nil, e
		}
	}
	return f, nil
}
func Open(root, environment, nip string) (*Store, error) {
	if environment != "test" && environment != "demo" && environment != "prod" {
		return nil, errors.New("invalid environment namespace")
	}
	if !regexp.MustCompile(`^[0-9]{10}$`).MatchString(nip) {
		return nil, errors.New("invalid NIP namespace")
	}
	root, e := filepath.Abs(root)
	if e != nil {
		return nil, e
	}
	if root == "/" {
		return nil, errors.New("storage root must be a private directory, not /")
	}
	parent, e := os.Open("/")
	if e != nil {
		return nil, e
	}
	parts := strings.Split(strings.TrimPrefix(root, "/"), "/")
	for i, p := range parts {
		next, err := openDir(parent, p, false)
		if errors.Is(err, syscall.ENOENT) {
			next, err = openDir(parent, p, true)
		}
		_ = parent.Close() // Read-only directory descriptor.
		if err != nil {
			return nil, err
		}
		parent = next
		if i == len(parts)-1 {
			if e = parent.Chmod(0700); e != nil {
				_ = parent.Close() // Preserve the chmod error.
				return nil, e
			}
		}
	}
	defer func() { _ = parent.Close() }() // Read-only directory descriptor.
	env, e := openDir(parent, environment, true)
	if e != nil {
		return nil, e
	}
	defer func() { _ = env.Close() }() // Read-only directory descriptor.
	ns, e := openDir(env, nip, true)
	if e != nil {
		return nil, e
	}
	defer func() { _ = ns.Close() }() // Read-only directory descriptor.
	s := &Store{Root: root, dir: filepath.Join(root, environment, nip)}
	ok := false
	defer func() {
		if !ok {
			_ = s.Close() // Preserve the original open error.
		}
	}()
	s.lock, e = openRegular(ns, ".lock", syscall.O_CREAT|syscall.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(s.lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		return nil, errors.New("another pylon sync holds this environment/NIP lock")
	}
	s.invoices, e = openDir(ns, "invoices", true)
	if e != nil {
		return nil, e
	}
	s.checkpoints, e = openDir(ns, "checkpoints", true)
	if e != nil {
		return nil, e
	}
	ok = true
	return s, nil
}
func (s *Store) Close() error {
	var result error
	for _, f := range []*os.File{s.invoices, s.checkpoints, s.lock} {
		if f != nil {
			result = errors.Join(result, f.Close())
		}
	}
	s.invoices = nil
	s.checkpoints = nil
	s.lock = nil
	return result
}
func openRegular(dir *os.File, name string, flags int, mode uint32) (*os.File, error) {
	fd, e := syscall.Openat(int(dir.Fd()), name, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, mode)
	if e != nil {
		return nil, fmt.Errorf("open storage file %s: %w", name, e)
	}
	f := os.NewFile(uintptr(fd), name)
	info, e := f.Stat()
	if e == nil && (!info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0) {
		e = errors.New("storage file must be regular and private (chmod 600)")
	}
	if e != nil {
		_ = f.Close() // Preserve the validation error.
		return nil, e
	}
	return f, nil
}
func readFile(dir *os.File, name string, limit int64) ([]byte, error) {
	f, e := openRegular(dir, name, syscall.O_RDONLY, 0)
	if e != nil {
		return nil, e
	}
	defer func() { _ = f.Close() }() // Read-only file.
	b, e := io.ReadAll(io.LimitReader(f, limit+1))
	if e == nil && int64(len(b)) > limit {
		e = errors.New("stored file exceeded size limit")
	}
	return b, e
}
func invoiceNames(n string) (string, string, error) {
	if !numberPattern.MatchString(n) {
		return "", "", errors.New("invalid KSeF number")
	}
	return n + ".xml", n + ".json", nil
}
func validSubject(s string) bool { return s == "received" || s == "issued" }

// SaveXML preserves response bytes and commits metadata only after durable XML.
func (s *Store) SaveXML(n string, b []byte, m Metadata) (bool, error) {
	return s.SaveXMLContext(context.Background(), n, b, m)
}
func (s *Store) SaveXMLContext(ctx context.Context, n string, b []byte, m Metadata) (bool, error) {
	if e := ctx.Err(); e != nil {
		return false, e
	}
	xp, mp, e := invoiceNames(n)
	if e != nil {
		return false, e
	}
	if e = validateXML(ctx, b); e != nil {
		return false, e
	}
	if m.PermanentStorageDate.IsZero() || !validSubject(m.Subject) {
		return false, errors.New("invoice metadata omitted storage date or subject")
	}
	sum := sha256.Sum256(b)
	if m.APIHash != "" && m.APIHash != base64.StdEncoding.EncodeToString(sum[:]) {
		return false, errors.New("invoice XML does not match KSeF invoice hash")
	}
	existing, e := readFile(s.invoices, xp, maxXML)
	xmlExists := e == nil
	if e != nil && !errors.Is(e, os.ErrNotExist) {
		return false, e
	}
	if xmlExists && !bytes.Equal(existing, b) {
		return false, fmt.Errorf("conflicting existing invoice %s", n)
	}
	old, metaExists, e := s.metadata(n, b)
	if e != nil {
		return false, e
	}
	metadataChanged := true
	if metaExists {
		before, _ := json.Marshal(old)
		m, e = mergeMetadata(old, m)
		if e != nil {
			return false, e
		}
		after, _ := json.Marshal(m)
		metadataChanged = !bytes.Equal(before, after)
	} else {
		m.KSeFNumber = n
		m.ByteLength = int64(len(b))
		m.SHA256 = hex.EncodeToString(sum[:])
		m.Subjects = []string{m.Subject}
		if m.DownloadedAt.IsZero() {
			m.DownloadedAt = time.Now().UTC()
		}
	}
	if !xmlExists {
		if e = atomicWrite(ctx, s.invoices, xp, b); e != nil {
			return false, e
		}
	}
	if !xmlExists || metadataChanged {
		if e = writeJSON(ctx, s.invoices, mp, m); e != nil {
			return false, e
		}
	}
	return !xmlExists || !metaExists, nil
}
func validateXML(ctx context.Context, b []byte) error {
	if len(b) == 0 || len(b) > maxXML {
		return errors.New("empty or oversized invoice body")
	}
	d := xml.NewDecoder(bytes.NewReader(bytes.TrimPrefix(b, []byte{0xef, 0xbb, 0xbf})))
	depth, roots, tokens := 0, 0, 0
	for {
		if e := ctx.Err(); e != nil {
			return e
		}
		token, e := d.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("invoice body is malformed XML")
		}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if roots != 1 || strings.EqualFold(t.Name.Local, "html") {
					return errors.New("invoice body must have one XML document root")
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return errors.New("invoice body has text outside XML root")
			}
		case xml.ProcInst:
			if strings.EqualFold(t.Target, "xml") && (t.Target != "xml" || tokens != 0) {
				return errors.New("invoice body has a misplaced XML declaration")
			}
		case xml.Directive:
			// Invoices need no DTD; reject it rather than interpret entities.
			return errors.New("invoice body contains an unsupported XML directive")
		}
		tokens++
	}
	if roots != 1 || depth != 0 {
		return errors.New("invoice body has no complete XML root")
	}
	return nil
}
func (s *Store) metadata(n string, b []byte) (Metadata, bool, error) {
	var m Metadata
	raw, e := readFile(s.invoices, n+".json", 1<<20)
	if errors.Is(e, os.ErrNotExist) {
		return m, false, nil
	}
	if e != nil {
		return m, false, e
	}
	if e = json.Unmarshal(raw, &m); e != nil {
		return m, false, fmt.Errorf("invalid invoice metadata: %w", e)
	}
	sum := sha256.Sum256(b)
	if m.KSeFNumber != n || m.PermanentStorageDate.IsZero() || m.DownloadedAt.IsZero() || !validSubject(m.Subject) || m.ByteLength != int64(len(b)) || m.SHA256 != hex.EncodeToString(sum[:]) || (m.APIHash != "" && m.APIHash != base64.StdEncoding.EncodeToString(sum[:])) {
		return m, false, fmt.Errorf("corrupt stored invoice or metadata %s", n)
	}
	for _, subject := range m.Subjects {
		if !validSubject(subject) {
			return m, false, errors.New("invalid stored subject provenance")
		}
	}
	return m, true, nil
}
func mergeMetadata(old, in Metadata) (Metadata, error) {
	if !in.PermanentStorageDate.IsZero() && !old.PermanentStorageDate.Equal(in.PermanentStorageDate) {
		return old, errors.New("invoice permanent storage date conflicts with sidecar")
	}
	if in.APIHash != "" {
		sum, e := hex.DecodeString(old.SHA256)
		if e != nil || in.APIHash != base64.StdEncoding.EncodeToString(sum) {
			return old, errors.New("invoice API hash conflicts with stored XML")
		}
		old.APIHash = in.APIHash
	}
	old.Subjects = append(old.Subjects, old.Subject)
	if in.Subject != "" {
		if !validSubject(in.Subject) {
			return old, errors.New("invalid subject provenance")
		}
		old.Subjects = append(old.Subjects, in.Subject)
	}
	slices.Sort(old.Subjects)
	old.Subjects = slices.Compact(old.Subjects)
	return old, nil
}
func (s *Store) ExistingValid(n string, expected ...Metadata) (bool, error) {
	xp, mp, e := invoiceNames(n)
	if e != nil {
		return false, e
	}
	b, e := readFile(s.invoices, xp, maxXML)
	if errors.Is(e, os.ErrNotExist) {
		// Even an orphan marker must not be a symlink/special file.
		_, me := readFile(s.invoices, mp, 1<<20)
		if me != nil && !errors.Is(me, os.ErrNotExist) {
			return false, me
		}
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if e = validateXML(context.Background(), b); e != nil {
		return false, e
	}
	m, exists, e := s.metadata(n, b)
	if e != nil || !exists {
		return false, e
	}
	if len(expected) > 0 {
		before, _ := json.Marshal(m)
		m, e = mergeMetadata(m, expected[0])
		if e != nil {
			return false, e
		}
		after, _ := json.Marshal(m)
		if !bytes.Equal(before, after) {
			if e = writeJSON(context.Background(), s.invoices, mp, m); e != nil {
				return false, e
			}
		}
	}
	return true, nil
}
func writeJSON(ctx context.Context, dir *os.File, name string, v any) error {
	b, e := json.MarshalIndent(v, "", "  ")
	if e != nil {
		return e
	}
	return atomicWrite(ctx, dir, name, append(b, '\n'))
}
func atomicWrite(ctx context.Context, dir *os.File, name string, b []byte) error {
	// Refuse existing symlinks and special files, even though rename would replace them.
	old, e := openRegular(dir, name, syscall.O_RDONLY, 0)
	if e == nil {
		_ = old.Close() // Read-only validation handle.
	} else if !errors.Is(e, os.ErrNotExist) {
		return e
	}
	temp := ".pylon-tmp-" + rand.Text()
	f, e := openRegular(dir, temp, syscall.O_CREAT|syscall.O_EXCL|syscall.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	// Best-effort cleanup; a successful rename already removed the temporary name.
	defer func() { _ = syscall.Unlinkat(int(dir.Fd()), temp) }()
	for len(b) > 0 && e == nil {
		if e = ctx.Err(); e != nil {
			break
		}
		n := min(len(b), 64<<10)
		var written int
		written, e = f.Write(b[:n])
		b = b[written:]
	}
	if e == nil {
		e = f.Sync()
	}
	e = errors.Join(e, f.Close())
	if e != nil {
		return e
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	if e = syscall.Renameat(int(dir.Fd()), temp, int(dir.Fd()), name); e != nil {
		return e
	}
	return dir.Sync()
}
func (s *Store) Checkpoint(subject string) (Checkpoint, bool, error) {
	var c Checkpoint
	if !validSubject(subject) {
		return c, false, errors.New("invalid checkpoint subject")
	}
	b, e := readFile(s.checkpoints, subject+".json", 1<<20)
	if errors.Is(e, os.ErrNotExist) {
		return c, false, nil
	}
	if e != nil {
		return c, false, e
	}
	if e = json.Unmarshal(b, &c); e != nil {
		return c, false, fmt.Errorf("invalid checkpoint: %w", e)
	}
	if e = s.validateCheckpoint(c, subject); e != nil {
		return c, false, e
	}
	return c, true, nil
}
func (s *Store) validateCheckpoint(c Checkpoint, subject string) error {
	if c.Version != CheckpointVersion || c.Environment != filepath.Base(filepath.Dir(s.dir)) || c.NIP != filepath.Base(s.dir) || c.Subject != subject || !validSubject(subject) || c.FilterVersion != 1 || c.Cursor.IsZero() {
		return errors.New("checkpoint namespace, cursor or version mismatch")
	}
	return nil
}
func (s *Store) SaveCheckpoint(c Checkpoint) error {
	c.Version = CheckpointVersion
	c.FilterVersion = 1
	if e := s.validateCheckpoint(c, c.Subject); e != nil {
		return e
	}
	return writeJSON(context.Background(), s.checkpoints, c.Subject+".json", c)
}
