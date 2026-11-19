package digest

import (
	"strings"
	"testing"
	"time"
)

func TestExportFreshnessAndMissingMonths(t *testing.T) {
	now := time.Date(2026, 10, 2, 8, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		updated time.Time
		want    string
	}{
		{"recent", now.Add(-time.Hour), "fresh"},
		{"boundary", now.Add(-72 * time.Hour), "fresh"},
		{"stale", now.Add(-72*time.Hour - time.Second), "stale"},
		{"missing", time.Time{}, "unknown"},
		{"future", now.Add(time.Hour), "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := ExportCoverage(tc.updated, now)
			if c.Freshness != tc.want || c.Completeness != "unverified" {
				t.Fatalf("%+v", c)
			}
		})
	}
	c := ExportCoverage(now, now)
	c.QualifyMonths([]Row{{Month: "2026-08", NetCost: 0}}, "2026-09", "2026-08")
	if c.Completeness != "partial" || !strings.Contains(c.Text(), "No rows for 2026-09") || strings.Contains(c.Text(), "No rows for 2026-08") {
		t.Fatal(c.Text())
	}
}

func TestNoSuccessfulAccountStillHasCoverage(t *testing.T) {
	got := summaryText([]Section{{Display: "Missing", AccountID: "synthetic", Error: "not configured"}})
	if !strings.Contains(got, "0/1 selected accounts queried") || !strings.Contains(got, "partial") {
		t.Fatal(got)
	}
}
