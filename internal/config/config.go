package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const CommandSync = "sync"

type Options struct {
	Help        bool
	Environment string
	Nip         string
	Subject     string
	From        string
	To          string
	StorageDir  string
	TokenFile   string
}

type Config struct {
	Environment string
	BaseURL     string
	Nip         string
	Subject     string
	From, To    *time.Time
	StorageDir  string
	Token       string
}

var nipPattern = regexp.MustCompile(`^[0-9]{10}$`)

const HelpText = `Usage:
  pylon sync --nip <10-digit-NIP> [options]

Options:
  --environment test|demo|prod   KSeF environment (default: test; prod is explicit)
  --nip <NIP>                    Taxpayer NIP
  --subject received|issued|both Invoice direction (default: received)
  --from <RFC3339>               Initial start; on later runs replays from this time
  --to <RFC3339>                 Optional bounded end (inclusive boundary)
  --storage-dir <dir>            Private data directory (default: ./data)
  --token-file <path>            Read KSeF token from a private file
  --help                         Show help without network access

Credential: set PYLON_KSEF_TOKEN or use --token-file. Never pass a token as an argument.
`

func ParseArgs(argv []string) (Options, error) {
	var o Options
	fs := flag.NewFlagSet("pylon", flag.ContinueOnError)
	fs.SetOutput(io.Discard) // The caller reports errors and prints HelpText.
	fs.BoolVar(&o.Help, "help", false, "Show help without network access")
	fs.BoolVar(&o.Help, "h", false, "Show help without network access")
	fs.StringVar(&o.Environment, "environment", "", "KSeF environment: test, demo, or prod")
	fs.StringVar(&o.Nip, "nip", "", "Taxpayer NIP")
	fs.StringVar(&o.Subject, "subject", "", "Invoice direction: received, issued, or both")
	fs.StringVar(&o.From, "from", "", "Initial start (RFC3339)")
	fs.StringVar(&o.To, "to", "", "Optional bounded end (RFC3339)")
	fs.StringVar(&o.StorageDir, "storage-dir", "", "Private data directory")
	fs.StringVar(&o.TokenFile, "token-file", "", "Read KSeF token from a private file")
	if err := fs.Parse(argv); err != nil {
		return o, err
	}
	args := fs.Args()
	if len(args) == 0 {
		if o.Help {
			return o, nil
		}
		return o, errors.New("expected command: sync")
	}
	command := args[0]
	if err := fs.Parse(args[1:]); err != nil {
		return o, err
	}
	if fs.NArg() != 0 {
		return o, fmt.Errorf("unexpected argument: %s", fs.Arg(0))
	}
	if !o.Help && command != CommandSync {
		return o, errors.New("expected command: sync")
	}
	return o, nil
}

func validNIP(s string) bool {
	if !nipPattern.MatchString(s) {
		return false
	}
	weights := [...]int{6, 5, 7, 2, 3, 4, 5, 6, 7}
	sum := 0
	for i, w := range weights {
		sum += int(s[i]-'0') * w
	}
	return sum%11 != 10 && sum%11 == int(s[9]-'0')
}

func Validate(o Options, envLookup func(string) string) (Config, error) {
	var c Config
	c.Environment = o.Environment
	if c.Environment == "" {
		c.Environment = "test"
	}
	switch c.Environment {
	case "test":
		c.BaseURL = "https://api-test.ksef.mf.gov.pl/v2"
	case "demo":
		c.BaseURL = "https://api-demo.ksef.mf.gov.pl/v2"
	case "prod":
		c.BaseURL = "https://api.ksef.mf.gov.pl/v2"
	default:
		return c, fmt.Errorf("invalid environment %q (expected test, demo, or prod)", c.Environment)
	}
	if !validNIP(o.Nip) {
		return c, errors.New("NIP must be a valid 10-digit Polish NIP")
	}
	c.Nip = o.Nip
	c.Subject = o.Subject
	if c.Subject == "" {
		c.Subject = "received"
	}
	if c.Subject != "received" && c.Subject != "issued" && c.Subject != "both" {
		return c, errors.New("subject must be received, issued, or both")
	}
	parse := func(s string) (*time.Time, error) {
		if s == "" {
			return nil, nil
		}
		t, e := time.Parse(time.RFC3339Nano, s)
		if e != nil {
			return nil, fmt.Errorf("invalid RFC3339 date %q", s)
		}
		u := t.UTC()
		return &u, nil
	}
	var err error
	c.From, err = parse(o.From)
	if err != nil {
		return c, err
	}
	c.To, err = parse(o.To)
	if err != nil {
		return c, err
	}
	if c.From != nil && c.To != nil && !c.From.Before(*c.To) {
		return c, errors.New("--from must be earlier than --to")
	}
	c.StorageDir = o.StorageDir
	if c.StorageDir == "" {
		c.StorageDir = "./data"
	}
	c.StorageDir = filepath.Clean(c.StorageDir)
	if envLookup == nil {
		envLookup = os.Getenv
	}
	c.Token = strings.TrimSpace(envLookup("PYLON_KSEF_TOKEN"))
	if o.TokenFile != "" {
		b, e := os.ReadFile(o.TokenFile)
		if e != nil {
			return c, fmt.Errorf("read token file: %w", e)
		}
		info, e := os.Stat(o.TokenFile)
		if e != nil {
			return c, e
		}
		if info.Mode().Perm()&0077 != 0 {
			return c, errors.New("token file must not be accessible by group or others (chmod 600)")
		}
		c.Token = strings.TrimSpace(string(b))
	}
	if c.Token == "" {
		return c, errors.New("missing credential: set PYLON_KSEF_TOKEN or --token-file")
	}
	return c, nil
}
