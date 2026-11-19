package gcp

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
)

// Collect discovers the open billing accounts, overlays the mappings and
// reads each export for report against the month before. Only a failure to
// list the accounts fails the collection; one account's failure becomes its
// section's error, which leaves the section incomplete.
func (a *API) Collect(ctx context.Context, log *slog.Logger, report string, exports []Export) ([]digest.Section, error) {
	t, err := time.Parse("2006-01", report)
	if err != nil {
		return nil, fmt.Errorf("month %q: want YYYY-MM", report)
	}
	prev := digest.MonthBefore(t)
	found, err := a.discover(ctx, log)
	if err != nil {
		return nil, err
	}
	var sections []digest.Section
	for _, acct := range mappedAccounts(found, exports, report) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		sections = append(sections, a.collectAccount(ctx, log, a.resolve(ctx, log, acct), report, prev))
	}
	return sections, nil
}

func (a *API) collectAccount(ctx context.Context, log *slog.Logger, acct account, report, prev string) digest.Section {
	s := digest.Section{Display: acct.Display, AccountID: acct.ID, ReportLabel: digest.MonthLabel(report), PrevLabel: digest.MonthLabel(prev)}
	s.Coverage = digest.ExportCoverage(acct.ModifiedAt, a.clock())
	if acct.Table == "" {
		s.Coverage.Completeness = "partial"
		s.Error = acct.Error
		if s.Error == "" {
			s.Error = notConfigured
		}
		s.Coverage.Details = append(s.Coverage.Details, s.Error)
		return s
	}
	id := ""
	if acct.Mapped {
		id = acct.ID
	}
	rows, currency, err := a.query(ctx, acct.Table, report, prev, id)
	if err != nil {
		s.Error = safe(err).Error()
		log.Warn("querying a billing export failed", "account", acct.ID, "table", acct.Table, "err", s.Error)
		s.Coverage.Completeness = "partial"
		s.Coverage.Details = append(s.Coverage.Details, "Query failed: "+s.Error)
		return s
	}
	s.Currency = currency
	s.Coverage.QualifyMonths(rows, report, prev)
	if s.Coverage.OnlyReportMonth(report) {
		earliest, err := a.earliestMonth(ctx, acct.Table, id)
		switch {
		case err != nil:
			log.Warn("reading a billing export's earliest month failed", "account", acct.ID, "table", acct.Table, "err", safe(err))
		case earliest == report:
			s.Coverage.BeginsIn(report, prev)
		}
	}
	if currency == "" {
		s.Coverage.Completeness = "partial"
		s.Error = "Currency unavailable; no amount included in subtotal"
		s.Coverage.Details = append(s.Coverage.Details, s.Error)
		return s
	}
	s.Fill(rows, report, prev)
	return s
}
