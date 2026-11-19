package digest

import (
	"encoding/json"
	"math"
	"os"
	"testing"
)

// sampleRows is the old tool's fixture: Mar/Feb 2026 across three projects
// and the NULL project, with a NULL service among them.
func sampleRows(t *testing.T) []Row {
	t.Helper()
	b, err := os.ReadFile("testdata/sample_bq_rows.json")
	if err != nil {
		t.Fatal(err)
	}
	var raw []struct {
		ProjectID   *string `json:"project_id"`
		ProjectName *string `json:"project_name"`
		ServiceID   *string `json:"service_id"`
		ServiceName *string `json:"service_name"`
		Month       string  `json:"month"`
		NetCost     float64 `json:"net_cost"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatal(err)
	}
	s := func(p *string) string {
		if p == nil {
			return ""
		}
		return *p
	}
	rows := make([]Row, len(raw))
	for i, r := range raw {
		rows[i] = Row{ProjectID: s(r.ProjectID), ProjectName: s(r.ProjectName), ServiceID: s(r.ServiceID),
			ServiceName: s(r.ServiceName), Month: r.Month, NetCost: r.NetCost}
	}
	return rows
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func byKey(lines []Line) map[string]Line {
	m := map[string]Line{}
	for _, l := range lines {
		m[l.Key] = l
	}
	return m
}

func TestPivotProjects(t *testing.T) {
	lines := pivotProjects(sampleRows(t), "2026-03", "2026-02")
	if len(lines) != 4 {
		t.Fatalf("%d lines, want 3 projects and the unattributed one", len(lines))
	}
	for i := 1; i < len(lines); i++ {
		if lines[i].Report > lines[i-1].Report {
			t.Fatalf("not sorted by report cost: %+v", lines)
		}
	}
	p := byKey(lines)
	for k, want := range map[string]float64{"alpha-main": 2.14, "beta": 0.21, "gamma": -0.03} {
		if !near(p[k].Delta, want) {
			t.Errorf("%s delta %v, want %v", k, p[k].Delta, want)
		}
	}
	if pct := p["alpha-main"].Pct; pct == nil || math.Abs(*pct-20.98) > 0.1 {
		t.Errorf("alpha pct %v", pct)
	}
	if u := p[""]; u.Name != unattributed {
		t.Errorf("NULL project named %q", u.Name)
	}
}

func TestPivotEdges(t *testing.T) {
	for _, tc := range []struct {
		name               string
		rows               []Row
		report, prev       string
		wantName           string
		wantReport, wantPv float64
		wantPct            *float64 // nil: no percentage
	}{
		{"new project", []Row{{ProjectID: "n", ProjectName: "new", Month: "2026-03", NetCost: 5}}, "2026-03", "2026-02", "new", 5, 0, nil},
		{"gone project", []Row{{ProjectID: "o", ProjectName: "old", Month: "2026-02", NetCost: 3}}, "2026-03", "2026-02", "old", 0, 3, new(float64(-100))},
		{"renamed", []Row{
			{ProjectID: "s", ProjectName: "old name", Month: "2026-02", NetCost: 5},
			{ProjectID: "s", ProjectName: "new name", Month: "2026-03", NetCost: 8},
		}, "2026-03", "2026-02", "new name", 8, 5, new(float64(60))},
		{"negative baseline", []Row{
			{Month: "2026-07", NetCost: -0.000579}, {Month: "2026-08", NetCost: 11.996773},
		}, "2026-08", "2026-07", unattributed, 11.996773, -0.000579, nil},
		{"sub-cent baseline", []Row{
			{Month: "2026-07", NetCost: 0.004797}, {Month: "2026-08", NetCost: -0.002686},
		}, "2026-08", "2026-07", unattributed, -0.002686, 0.004797, nil},
		{"one-cent baseline", []Row{
			{ProjectID: "p", ProjectName: "p", Month: "2026-07", NetCost: 0.01}, {ProjectID: "p", ProjectName: "p", Month: "2026-08", NetCost: 0.02},
		}, "2026-08", "2026-07", "p", 0.02, 0.01, new(float64(100))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lines := pivotProjects(tc.rows, tc.report, tc.prev)
			if len(lines) != 1 {
				t.Fatalf("%d lines", len(lines))
			}
			l := lines[0]
			if l.Name != tc.wantName || !near(l.Report, tc.wantReport) || !near(l.Prev, tc.wantPv) || !near(l.Delta, tc.wantReport-tc.wantPv) {
				t.Fatalf("%+v", l)
			}
			switch {
			case tc.wantPct == nil && l.Pct != nil:
				t.Fatalf("pct %v, want none", *l.Pct)
			case tc.wantPct != nil && (l.Pct == nil || !near(*l.Pct, *tc.wantPct)):
				t.Fatalf("pct %v, want %v", l.Pct, *tc.wantPct)
			}
		})
	}
}

func TestPivotServices(t *testing.T) {
	rows := sampleRows(t)
	s := byKey(pivotServices(rows, "2026-03", "2026-02"))
	for k, want := range map[string][2]float64{
		"COMP": {10, 9}, "BQ": {2.34, 1.20}, "RUN": {3.21, 3}, "GCS": {1, 1}, "": {0.87, 0.90}, "SUPPORT": {2, 2},
	} {
		if !near(s[k].Report, want[0]) || !near(s[k].Prev, want[1]) {
			t.Errorf("%q: %v/%v, want %v", k, s[k].Report, s[k].Prev, want)
		}
	}
	if s[""].Name != unknownService {
		t.Errorf("NULL service named %q", s[""].Name)
	}
	// One query, two pivots: the totals agree by construction.
	var pr, pp, sr, sp float64
	for _, l := range pivotProjects(rows, "2026-03", "2026-02") {
		pr, pp = pr+l.Report, pp+l.Prev
	}
	for _, l := range s {
		sr, sp = sr+l.Report, sp+l.Prev
	}
	if !near(pr, sr) || !near(pp, sp) {
		t.Fatalf("projects %v/%v, services %v/%v", pr, pp, sr, sp)
	}
	renamed := pivotServices([]Row{
		{ServiceID: "OLD", ServiceName: "Old Name", Month: "2026-02", NetCost: 5},
		{ServiceID: "OLD", ServiceName: "New Name", Month: "2026-03", NetCost: 7},
	}, "2026-03", "2026-02")
	if len(renamed) != 1 || renamed[0].Name != "New Name" {
		t.Fatalf("rename: %+v", renamed)
	}
	invoice := pivotServices([]Row{
		{ServiceID: "INV", ServiceName: "Invoice", Month: "2026-07", NetCost: -0.000766},
		{ServiceID: "INV", ServiceName: "Invoice", Month: "2026-08", NetCost: 0.003321},
	}, "2026-08", "2026-07")
	if invoice[0].Pct != nil {
		t.Fatal("a negative baseline must carry no percentage")
	}
}

func TestWithTail(t *testing.T) {
	svc := func(name string, report, prev float64) Line {
		d, p := delta(report, prev)
		return Line{Key: name, Name: name, Report: report, Prev: prev, Delta: d, Pct: p}
	}
	few := []Line{svc("A", 10, 0), svc("B", 5, 0), svc("C", 1, 0)}
	if got := withTail(few, 10); len(got) != 3 || got[2].Tail {
		t.Fatalf("below the threshold: %+v", got)
	}
	var many []Line
	for i := range 15 {
		many = append(many, svc(string(rune('a'+i)), float64(100-i), 0))
	}
	got := withTail(many, 10)
	if len(got) != 11 {
		t.Fatalf("%d lines", len(got))
	}
	other := got[10]
	if !other.Tail || other.Key != "" || other.Name != "Other (5 services)" || !near(other.Report, 90+89+88+87+86) || other.Pct != nil {
		t.Fatalf("tail %+v", other)
	}
	// Ranked by the month's cost, never the change: a new service leads.
	pair := []Line{svc("BrandNew", 1000, 0), svc("Established", 50, 48)}
	got = withTail(pair, 1)
	if got[0].Name != "BrandNew" || got[1].Name != "Other (1 service)" {
		t.Fatalf("%+v", got)
	}
}

func TestTotalUsesTheDisplayedBaseline(t *testing.T) {
	// The old tool printed a huge percentage here; the rows never did.
	tot := total([]Line{{Report: 12, Prev: -0.000579}})
	if tot.Pct != nil || !near(tot.Delta, 12.000579) {
		t.Fatalf("%+v", tot)
	}
	tot = total([]Line{{Report: 12.34, Prev: 10.20}, {Report: 4.21, Prev: 4}})
	if tot.Pct == nil || math.Abs(*tot.Pct-16.55) > 0.01 {
		t.Fatalf("%+v", tot)
	}
}
