package config

import (
	"errors"
	"fmt"
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
	seen := map[string]bool{}
	command := ""
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		if a == "-h" || a == "--help" {
			o.Help = true
			continue
		}
		if strings.HasPrefix(a, "--") {
			name, val, has := strings.Cut(a, "=")
			if seen[name] {
				return o, fmt.Errorf("duplicate flag: %s", name)
			}
			seen[name] = true
			var dst *string
			switch name {
			case "--environment":
				dst = &o.Environment
			case "--nip":
				dst = &o.Nip
			case "--subject":
				dst = &o.Subject
			case "--from":
				dst = &o.From
			case "--to":
				dst = &o.To
			case "--storage-dir":
				dst = &o.StorageDir
			case "--token-file":
				dst = &o.TokenFile
			default:
				return o, fmt.Errorf("unknown flag: %s", name)
			}
			if !has {
				if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "-") {
					return o, fmt.Errorf("%s requires a value", name)
				}
				i++
				val = argv[i]
			}
			*dst = val
			continue
		}
		if command != "" {
			return o, fmt.Errorf("unexpected argument: %s", a)
		}
		command = a
	}
	if o.Help {
		return o, nil
	}
	if command != "sync" {
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
