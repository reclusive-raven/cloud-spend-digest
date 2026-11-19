package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
	"github.com/reclusive-raven/cloud-spend-digest/gcp"
)

type fakeProvider struct {
	sections []digest.Section
	month    string
}

func (f *fakeProvider) Collect(_ context.Context, _ *slog.Logger, report string, _ []gcp.Export) ([]digest.Section, error) {
	f.month = report
	return f.sections, nil
}

func (f *fakeProvider) Check(context.Context, io.Writer, *slog.Logger, string, []gcp.Export) error {
	return nil
}

func section(display string, complete bool) digest.Section {
	s := digest.Section{Display: display, AccountID: "AAAAAA-AAAAAA-AAAAAA", Currency: "USD", ReportLabel: "Oct 2026", PrevLabel: "Sep 2026",
		Coverage: digest.Coverage{Completeness: "unverified"}}
	s.Fill([]digest.Row{{ProjectID: "p", ProjectName: "P", ServiceID: "S", ServiceName: "Svc", Month: "2026-10", NetCost: 1234.56},
		{ProjectID: "p", ProjectName: "P", ServiceID: "S", ServiceName: "Svc", Month: "2026-09", NetCost: 1000}}, "2026-10", "2026-09")
	if !complete {
		s.Coverage.Completeness = "partial"
		s.Coverage.Details = []string{"No rows for 2026-10; missing evidence is not confirmed zero spend"}
	}
	return s
}

type sent struct {
	to      []string
	subject string
	html    string
}

func runWith(t *testing.T, p *fakeProvider, args ...string) (string, string, *sent, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	var got *sent
	now := time.Date(2026, 11, 2, 2, 30, 0, 0, time.UTC)
	err := run(context.Background(), args, &out, &errOut, now,
		func(context.Context, string) (provider, error) { return p, nil },
		func(to []string, subject, html string, _ time.Time) error {
			got = &sent{to, subject, html}
			return nil
		})
	return out.String(), errOut.String(), got, err
}

func TestDefaultMonthIsLastMonthAndPrintsHTML(t *testing.T) {
	p := &fakeProvider{sections: []digest.Section{section("Main", true)}}
	out, _, got, err := runWith(t, p, "--project", "q")
	if err != nil || got != nil || p.month != "2026-10" {
		t.Fatalf("err %v, sent %v, month %q", err, got, p.month)
	}
	if !strings.Contains(out, "GCP spend — Oct 2026") || !strings.Contains(out, "1234.56") {
		t.Fatal(out)
	}
}

func TestIncompleteReadSendsAndPrintsNothing(t *testing.T) {
	p := &fakeProvider{sections: []digest.Section{section("Main", true), section("Other", false)}}
	for _, args := range [][]string{{"--month", "2026-10"}, {"--month", "2026-10", "--to", "a@example.test"}} {
		out, errOut, got, err := runWith(t, p, args...)
		if err == nil || got != nil || out != "" {
			t.Fatalf("%v: err %v, sent %v, out %q", args, err, got, out)
		}
		msg := err.Error() + errOut
		if !strings.Contains(msg, "Other (AAAAAA-AAAAAA-AAAAAA)") || strings.Contains(msg, "1234") || strings.Contains(msg, "1000") {
			t.Fatalf("error must name the account and carry no figures: %s", msg)
		}
	}
}

func TestSendPrintsNoFigures(t *testing.T) {
	p := &fakeProvider{sections: []digest.Section{section("Main", true)}}
	out, _, got, err := runWith(t, p, "--month", "2026-10", "--to", "a@example.test, b@example.test")
	if err != nil || got == nil {
		t.Fatalf("err %v, sent %v", err, got)
	}
	if len(got.to) != 2 || got.subject != "GCP spend — Oct 2026" || !strings.Contains(got.html, "1234.56") {
		t.Fatalf("%+v", got)
	}
	if strings.Contains(out, "1234") || strings.Contains(out, "<") {
		t.Fatalf("stdout carries the digest: %q", out)
	}
}

func TestUsageErrors(t *testing.T) {
	p := &fakeProvider{}
	for _, args := range [][]string{{"--month", "October"}, {"check", "--to", "a@example.test"}, {"extra"}} {
		if _, _, _, err := runWith(t, p, args...); err == nil {
			t.Errorf("%v accepted", args)
		} else if _, ok := err.(usageError); !ok {
			t.Errorf("%v: %v is not a usage error", args, err)
		}
	}
}

func TestBuildMessage(t *testing.T) {
	msg, err := buildMessage("from@example.test", []string{"a@example.test"}, "GCP spend — Oct 2026", "<p>hi</p>", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s := string(msg)
	for _, want := range []string{"From: from@example.test\r\n", "To: a@example.test\r\n", "Subject: =?utf-8?q?", "multipart/alternative", "text/html"} {
		if !strings.Contains(s, want) {
			t.Errorf("message missing %q", want)
		}
	}
}
