package digest

import (
	"fmt"
	"sort"
	"strconv"
)

const (
	unattributed   = "(unattributed)"
	unknownService = "(unknown service)"
	// topServices is how many services a section lists before the rest
	// collapse into one "Other" line.
	topServices = 10
)

// Line is one row of a table: a project, a service, the "Other" tail or a
// total, with the report month against the month before.
type Line struct {
	// Key is the project or service id; empty for a NULL id.
	Key          string
	Name         string
	Report, Prev float64
	Delta        float64
	// Pct is nil when the baseline cannot anchor a percentage (see delta).
	Pct *float64
	// Tail marks the "Other (n services)" line.
	Tail bool
	// NoComparison means Prev is no figure: the export holds no month before.
	NoComparison bool `json:",omitempty"`
}

// pivotProjects makes one line per project. Projects and services come off
// the same rows, so their totals agree by construction.
func pivotProjects(rows []Row, report, prev string) []Line {
	return pivot(rows, report, prev, unattributed, func(r Row) (string, string) { return r.ProjectID, r.ProjectName })
}

// pivotServices makes one line per service.
func pivotServices(rows []Row, report, prev string) []Line {
	return pivot(rows, report, prev, unknownService, func(r Row) (string, string) { return r.ServiceID, r.ServiceName })
}

// pivot sums each key's cost per month at full precision. The display name
// prefers the report month's (a mid-month rename), then the month before's;
// a NULL name is the fallback. Rows for any other month are ignored. The
// lines come out largest first by (report, prev), ties in first-seen order.
func pivot(rows []Row, report, prev, fallback string, key func(Row) (string, string)) []Line {
	type acc struct {
		report, prev         float64
		reportName, prevName string
		inReport, inPrev     bool
	}
	var order []string
	accs := map[string]*acc{}
	for _, r := range rows {
		if r.Month != report && r.Month != prev {
			continue
		}
		k, name := key(r)
		if name == "" {
			name = fallback
		}
		a, ok := accs[k]
		if !ok {
			a = &acc{}
			accs[k] = a
			order = append(order, k)
		}
		switch r.Month {
		case report:
			a.report += r.NetCost
			a.reportName, a.inReport = name, true
		case prev:
			a.prev += r.NetCost
			a.prevName, a.inPrev = name, true
		}
	}
	lines := make([]Line, 0, len(order))
	for _, k := range order {
		a := accs[k]
		name := fallback
		switch {
		case a.inReport:
			name = a.reportName
		case a.inPrev:
			name = a.prevName
		}
		d, pct := delta(a.report, a.prev)
		lines = append(lines, Line{Key: k, Name: name, Report: a.report, Prev: a.prev, Delta: d, Pct: pct})
	}
	sortLines(lines)
	return lines
}

func sortLines(lines []Line) {
	sort.SliceStable(lines, func(i, j int) bool {
		if lines[i].Report != lines[j].Report {
			return lines[i].Report > lines[j].Report
		}
		return lines[i].Prev > lines[j].Prev
	})
}

// delta is the change, and the percentage when the baseline supports one:
// only when the baseline as displayed (to the cent) is positive. A negative
// baseline flips the ratio's sign against the change beside it, and a
// sub-cent one explodes: Google's "Invoice" line carries sub-cent rounding
// adjustments that flip sign each month, and against -0.000579 the August
// 2026 digest once printed -2072082% next to a column reading 0.00.
func delta(report, prev float64) (float64, *float64) {
	d := report - prev
	if round2(prev) <= 0 {
		return d, nil
	}
	pct := d / prev * 100
	return d, &pct
}

// withTail keeps the top n services and folds the rest into one line.
// Services are already ordered by report-month cost, never by change, so a
// new service that dominates the month cannot hide in the tail.
func withTail(services []Line, n int) []Line {
	if len(services) <= n {
		return services
	}
	tail := services[n:]
	var report, prev float64
	for _, s := range tail {
		report += s.Report
		prev += s.Prev
	}
	d, pct := delta(report, prev)
	noun := "services"
	if len(tail) == 1 {
		noun = "service"
	}
	other := Line{Name: fmt.Sprintf("Other (%d %s)", len(tail), noun), Report: report, Prev: prev, Delta: d, Pct: pct, Tail: true}
	return append(append([]Line{}, services[:n]...), other)
}

// total sums the project lines.
func total(projects []Line) Line {
	var report, prev float64
	for _, p := range projects {
		report += p.Report
		prev += p.Prev
	}
	d, pct := delta(report, prev)
	return Line{Name: "TOTAL", Report: report, Prev: prev, Delta: d, Pct: pct}
}

// round2 rounds to the cent the way the old tool's Python round(x, 2) did:
// correctly, on the float's exact value.
func round2(v float64) float64 {
	r, _ := strconv.ParseFloat(strconv.FormatFloat(v, 'f', 2, 64), 64)
	return r
}
