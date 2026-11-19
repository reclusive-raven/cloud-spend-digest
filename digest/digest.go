// Package digest turns cost rows into the monthly email. It knows nothing
// about any cloud provider: a provider package collects Sections and this
// package pivots, checks and renders them.
package digest

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Row is one (project, service, month) group of a billing export. An empty
// id or name is a NULL in the export.
type Row struct {
	ProjectID, ProjectName string
	ServiceID, ServiceName string
	// Month is YYYY-MM.
	Month   string
	NetCost float64
}

// Section is one billing account in the email.
type Section struct {
	Display   string
	AccountID string
	// Currency is the export's; empty on an error section.
	Currency    string
	ReportLabel string
	PrevLabel   string
	Projects    []Line
	Services    []Line
	Total       Line
	// Error replaces the tables: the export is missing or the query failed.
	Error    string
	Coverage Coverage
}

// Complete reports whether the section is fit to send. An export that began
// in the report month is complete: it has no comparison, not a gap.
func (s Section) Complete() bool {
	return s.Error == "" && s.Currency != "" && s.Coverage.Completeness != "partial"
}

// Fill pivots rows into the section's tables. When the coverage says there
// is no comparison, every line's previous month is blanked rather than zero.
func (s *Section) Fill(rows []Row, report, prev string) {
	s.Projects = pivotProjects(rows, report, prev)
	s.Services = withTail(pivotServices(rows, report, prev), topServices)
	s.Total = total(s.Projects)
	if s.Coverage.NoComparison {
		s.Total.NoComparison = true
		for _, lines := range [][]Line{s.Projects, s.Services} {
			for i := range lines {
				lines[i].NoComparison = true
			}
		}
	}
}

// Coverage separates evidence availability from freshness. A readable export
// is never proof that an invoice or the visible inventory is complete.
type Coverage struct {
	Completeness string
	Freshness    string
	CheckedAt    time.Time
	UpdatedAt    time.Time
	Details      []string
	MonthGroups  []string
	// NoComparison marks an export whose history begins in the report month.
	NoComparison bool
}

// ExportMaxAge is how old an export table may be and still count as fresh.
const ExportMaxAge = 72 * time.Hour

// ExportCoverage starts an account's coverage from its export table's
// modification time.
func ExportCoverage(updated, now time.Time) Coverage {
	c := Coverage{Completeness: "unverified", Freshness: "unknown", CheckedAt: now.UTC(), UpdatedAt: updated}
	if !updated.IsZero() && !updated.After(now) {
		c.Freshness = "fresh"
		if now.Sub(updated) > ExportMaxAge {
			c.Freshness = "stale"
		}
	}
	c.Details = []string{"Freshness uses export table modification time, not an invoice-finality watermark; export completeness and accounts outside the visible/configured inventory are unverified"}
	return c
}

// Text is the coverage line printed under an account's heading.
func (c Coverage) Text() string {
	completeness, freshness := c.Completeness, c.Freshness
	if completeness == "" {
		completeness = "unverified"
	}
	if freshness == "" {
		freshness = "unknown"
	}
	text := "Coverage: " + completeness + "; freshness: " + freshness
	if !c.UpdatedAt.IsZero() {
		text += "; export table modified " + c.UpdatedAt.UTC().Format(time.RFC3339)
	}
	if !c.CheckedAt.IsZero() {
		text += "; checked " + c.CheckedAt.UTC().Format(time.RFC3339)
	}
	if len(c.Details) > 0 {
		text += ". " + strings.Join(c.Details, "; ")
	}
	if len(c.MonthGroups) > 0 {
		text += ". Invoice-month groups present: " + strings.Join(c.MonthGroups, ", ") + "; daily coverage unavailable"
	}
	return text
}

// QualifyMonths marks each of the two months that has no rows. Missing
// evidence is never read as zero spend.
func (c *Coverage) QualifyMonths(rows []Row, report, prev string) {
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r.Month] = true
	}
	for _, month := range []string{report, prev} {
		if seen[month] {
			c.MonthGroups = append(c.MonthGroups, month)
		} else {
			c.Completeness = "partial"
			c.Details = append(c.Details, "No rows for "+month+"; missing evidence is not confirmed zero spend")
		}
	}
}

// OnlyReportMonth reports whether the rows held the report month but not the
// one before, the case where the export may have begun this month.
func (c Coverage) OnlyReportMonth(report string) bool {
	return slices.Equal(c.MonthGroups, []string{report})
}

// BeginsIn replaces QualifyMonths' verdict for an export with no history
// before the report month: the month before can never appear, so it is no gap
// to wait on, though nothing says the spend before the export began was zero.
func (c *Coverage) BeginsIn(report, prev string) {
	c.Completeness, c.NoComparison = "unverified", true
	for i, detail := range c.Details {
		if strings.HasPrefix(detail, "No rows for "+prev) {
			c.Details[i] = "No rows for " + prev + ": this export holds nothing before " + report + ", so there is no comparison; missing evidence is not confirmed zero spend"
		}
	}
}

// summaryText is the coverage line at the top of the email, over every account.
func summaryText(sections []Section) string {
	if len(sections) == 0 {
		return "Coverage: unverified; freshness: unknown."
	}
	available := 0
	state, freshness := "unverified", "fresh"
	var details []string
	var checked time.Time
	for _, s := range sections {
		ok := s.Error == "" && s.Currency != ""
		c := s.Coverage
		if ok {
			available++
		}
		if !ok || c.Completeness == "partial" {
			state = "partial"
		}
		switch c.Freshness {
		case "stale":
			freshness = "stale"
		case "fresh":
		default:
			if freshness != "stale" {
				freshness = "unknown"
			}
		}
		if !c.CheckedAt.IsZero() && (checked.IsZero() || c.CheckedAt.Before(checked)) {
			checked = c.CheckedAt
		}
		name := s.Display + " (" + s.AccountID + ")"
		if !ok {
			details = append(details, "Unavailable account: "+name)
		}
		if c.Freshness == "stale" {
			details = append(details, "Stale export: "+name)
		}
		if ok && (c.Completeness == "partial" || c.NoComparison) {
			for _, detail := range c.Details {
				if strings.HasPrefix(detail, "No rows for ") {
					details = append(details, name+": "+detail)
				}
			}
		}
	}
	if !checked.IsZero() {
		details = append(details, "Evidence checked "+checked.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("Known export subtotal; coverage: %s; freshness at collection: %s; %d/%d selected accounts queried. %s", state, freshness, available, len(sections), strings.Join(details, " · "))
}

// MonthBefore is the month before t's, as YYYY-MM.
func MonthBefore(t time.Time) string {
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC).AddDate(0, -1, 0).Format("2006-01")
}

// MonthLabel is "Aug 2026" for "2026-08".
func MonthLabel(month string) string {
	t, err := time.Parse("2006-01", month)
	if err != nil {
		return month
	}
	return t.Format("Jan 2006")
}

// Subject is the email's subject line for a report month.
func Subject(provider, report string) string { return provider + " spend — " + MonthLabel(report) }
