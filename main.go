// Command cloud-spend-digest builds one month's cloud spend digest from the
// provider's billing export and prints it as HTML or emails it.
//
//	cloud-spend-digest [--month YYYY-MM] [--project P] [--exports FILE] [--to a@x,b@y]
//	cloud-spend-digest check [--month YYYY-MM] [--project P] [--exports FILE]
//
// It runs once and keeps no state. If any account's export cannot be read in
// full, it prints and sends nothing and exits 1.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"time"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
	"github.com/reclusive-raven/cloud-spend-digest/gcp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	err := run(ctx, os.Args[1:], os.Stdout, os.Stderr, time.Now(), collector(gcp.New), smtpSender)
	if errors.Is(err, flag.ErrHelp) {
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "cloud-spend-digest:", err)
		var usage usageError
		if errors.As(err, &usage) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

type usageError struct{ error }

// provider is what run needs from a cloud: the sections for a month and a
// readiness check. Only gcp implements it today.
type provider interface {
	Collect(ctx context.Context, log *slog.Logger, report string, exports []gcp.Export) ([]digest.Section, error)
	Check(ctx context.Context, out io.Writer, log *slog.Logger, report string, exports []gcp.Export) error
}

type connect func(ctx context.Context, project string) (provider, error)

func collector(newAPI func(context.Context, string) (*gcp.API, error)) connect {
	return func(ctx context.Context, project string) (provider, error) { return newAPI(ctx, project) }
}

type sender func(to []string, subject, html string, now time.Time) error

func smtpSender(to []string, subject, html string, now time.Time) error {
	c, err := smtpFromEnv()
	if err != nil {
		return err
	}
	msg, err := buildMessage(c.From, to, subject, html, now)
	if err != nil {
		return err
	}
	return send(c, to, msg)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer, now time.Time, connect connect, send sender) error {
	checking := len(args) > 0 && args[0] == "check"
	if checking {
		args = args[1:]
	}
	fs := flag.NewFlagSet("cloud-spend-digest", flag.ContinueOnError)
	fs.SetOutput(stderr)
	month := fs.String("month", digest.MonthBefore(now), "report month, YYYY-MM (default last month)")
	project := fs.String("project", os.Getenv("GOOGLE_CLOUD_PROJECT"), "project that runs and pays for the BigQuery jobs (default $GOOGLE_CLOUD_PROJECT)")
	exportsFile := fs.String("exports", "", "optional JSON list of {account_id, display, table, from_month, through_month} mappings")
	to := fs.String("to", "", "comma-separated recipients; email the digest instead of printing it (SMTP_* environment)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return usageError{err}
	}
	if fs.NArg() > 0 {
		return usageError{fmt.Errorf("unexpected argument %q", fs.Arg(0))}
	}
	if _, err := time.Parse("2006-01", *month); err != nil {
		return usageError{errors.New("--month must be YYYY-MM")}
	}
	var recipients []string
	for _, r := range strings.Split(*to, ",") {
		if r = strings.TrimSpace(r); r != "" {
			recipients = append(recipients, r)
		}
	}
	if checking && len(recipients) > 0 {
		return usageError{errors.New("check sends nothing; drop --to")}
	}
	exports, err := readExports(*exportsFile)
	if err != nil {
		return usageError{err}
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	p, err := connect(ctx, *project)
	if err != nil {
		return err
	}
	if checking {
		return p.Check(ctx, stdout, log, *month, exports)
	}
	sections, err := p.Collect(ctx, log, *month, exports)
	if err != nil {
		return err
	}
	if len(sections) == 0 {
		return errors.New("no billing accounts are visible; run `cloud-spend-digest check`")
	}
	// No partial digest: an account missing from the figures reads as no spend.
	var incomplete []string
	for _, s := range sections {
		if !s.Complete() {
			reason := s.Error
			if reason == "" {
				reason = s.Coverage.Text()
			}
			incomplete = append(incomplete, fmt.Sprintf("%s (%s): %s", s.Display, s.AccountID, reason))
		}
	}
	if len(incomplete) > 0 {
		return fmt.Errorf("incomplete read, nothing sent; run `cloud-spend-digest check`:\n  %s", strings.Join(incomplete, "\n  "))
	}
	subject := digest.Subject("GCP", *month)
	html, err := digest.Render(subject, "BigQuery jobs run in "+*project+".", sections)
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		_, err := io.WriteString(stdout, html)
		return err
	}
	if err := send(recipients, subject, html, now); err != nil {
		return err
	}
	_, err = fmt.Fprintf(stdout, "sent %s to %d recipient(s)\n", *month, len(recipients))
	return err
}

func readExports(path string) ([]gcp.Export, error) {
	if path == "" {
		return nil, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var exports []gcp.Export
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&exports); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return exports, gcp.ValidateExports(exports)
}
