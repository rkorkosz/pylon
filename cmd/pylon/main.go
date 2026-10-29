package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	osSignal "os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/rkrokosz/pylon/internal/calendar"
	"github.com/rkrokosz/pylon/internal/config"
	"github.com/rkrokosz/pylon/internal/ksef"
	"github.com/rkrokosz/pylon/internal/storage"
	syncer "github.com/rkrokosz/pylon/internal/sync"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "pylon:", err)
		os.Exit(1)
	}
}
func run(args []string) error {
	ctx, stop := osSignal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return runCLI(ctx, args, os.Stdout, func(c config.Config) *ksef.Client { return ksef.New(c.BaseURL, c.Nip) })
}

// The client factory is a test seam, not a configurable credential destination.
func runCLI(ctx context.Context, args []string, out io.Writer, newClient func(config.Config) *ksef.Client) (result error) {
	o, err := config.ParseArgs(args)
	if err != nil {
		return err
	}
	if o.Help {
		_, err := fmt.Fprint(out, config.HelpText)
		return err
	}
	c, err := config.Validate(o, nil)
	if err != nil {
		return err
	}
	subjects := []string{c.Subject}
	if c.Subject == "both" {
		subjects = []string{"received", "issued"}
	}
	store, err := storage.Open(c.StorageDir, c.Environment, c.Nip)
	if err != nil {
		return err
	}
	defer func() { result = errors.Join(result, store.Close()) }()
	if o.Command == "export-ics" {
		b, err := calendar.Export(ctx, store, c.Environment, c.Nip, c.Subject)
		if err != nil {
			return err
		}
		_, err = out.Write(b)
		return err
	}
	for _, s := range subjects {
		_, ok, e := store.Checkpoint(s)
		if e != nil {
			return e
		}
		if !ok && c.From == nil {
			return fmt.Errorf("first sync for %s requires --from", s)
		}
	}
	api := newClient(c)
	if err = api.Authenticate(ctx, c.Token); err != nil {
		return fmt.Errorf("KSeF authentication: %w", err)
	}
	summaries, err := syncer.Run(ctx, api, store, c.Environment, c.Nip, subjects, c.From, c.To)
	for _, s := range summaries {
		boundary := "none"
		if !s.CompletedThrough.IsZero() {
			boundary = s.CompletedThrough.Format(time.RFC3339Nano)
		}
		if _, writeErr := fmt.Fprintf(out, "subject=%s discovered=%d downloaded=%d already_present=%d failed=%d completed_through=%s", s.Subject, s.Discovered, s.Downloaded, s.AlreadyPresent, s.Failed, boundary); writeErr != nil {
			return errors.Join(err, writeErr)
		}
		if !s.DeferredTo.IsZero() {
			if _, writeErr := fmt.Fprintf(out, " deferred=%s..%s", s.DeferredFrom.Format(time.RFC3339Nano), s.DeferredTo.Format(time.RFC3339Nano)); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		if _, writeErr := fmt.Fprintln(out); writeErr != nil {
			return errors.Join(err, writeErr)
		}
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return errors.New("sync cancelled; completed checkpoints are durable and the unfinished window will resume on next run")
		}
		if strings.Contains(err.Error(), "incomplete") {
			return err
		}
		return fmt.Errorf("sync failed; keep data and rerun to resume: %w", err)
	}
	return nil
}
