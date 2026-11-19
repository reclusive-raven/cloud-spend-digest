package digest

import (
	"strings"
	"testing"
)

func testSection() Section {
	line := func(key, name string, report, prev, pct float64) Line {
		return Line{Key: key, Name: name, Report: report, Prev: prev, Delta: report - prev, Pct: &pct}
	}
	return Section{
		Display: "My Billing Account", AccountID: "AAAAAA-AAAAAA-AAAAAA", Currency: "USD",
		ReportLabel: "Mar 2026", PrevLabel: "Feb 2026",
		Projects: []Line{
			line("alpha-main", "main", 12.34, 10.20, 20.98),
			line("beta", "beta", 4.21, 4.00, 5.25),
			line("gamma", "gamma", 0.87, 0.90, -3.33),
			line("", "(unattributed)", 2.00, 2.00, 0),
		},
		Total: line("", "TOTAL", 19.42, 17.10, 13.57),
	}
}

func mustRender(t *testing.T, sections ...Section) string {
	t.Helper()
	html, err := Render("GCP spend — Mar 2026", "BigQuery jobs run in alpha-main.", sections)
	if err != nil {
		t.Fatal(err)
	}
	return html
}

func TestRenderDigest(t *testing.T) {
	html := mustRender(t, testSection())
	for _, want := range []string{
		"GCP spend — Mar 2026", "My Billing Account", "AAAAAA-AAAAAA-AAAAAA", "&mdash; USD",
		"main", "beta", "gamma", "(unattributed)", "12.34", "10.20", "EXPORT SUBTOTAL", "19.42",
		"+21%", "-3%", `background-color: #ffffff;`,
		`<meta name="color-scheme" content="light">`, `<meta name="supported-color-schemes" content="light">`,
		"Sent by cloud-spend-digest", "BigQuery jobs run in alpha-main.",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(html, "Top services") || strings.Contains(html, "cloud-billing-digest") {
		t.Error("no services table without services, and no link to the retired repo")
	}
	// The month before comes first, in the header and in each row.
	table := html[strings.Index(html, "<table>"):]
	if strings.Index(table, "Feb 2026") > strings.Index(table, "Mar 2026") {
		t.Error("prev month column must come first")
	}
	row := html[strings.Index(html, "main"):strings.Index(html, "beta")]
	if strings.Index(row, "10.20") > strings.Index(row, "12.34") {
		t.Error("prev cost must come first in a row")
	}
	// A shaded header pins its text colour against half-inverting clients.
	th := html[strings.Index(html, "    th {"):]
	if th = th[:strings.Index(th, "\n")]; !strings.Contains(th, "color: #222") {
		t.Errorf("th rule %q", th)
	}
}

func TestRenderServicesAndTail(t *testing.T) {
	s := testSection()
	s.Services = []Line{
		{Key: "COMP", Name: "Compute Engine", Report: 19, Prev: 15, Delta: 4, Pct: new(26.67)},
		{Name: "Other (3 services)", Report: 1, Prev: 1, Tail: true},
	}
	html := mustRender(t, s)
	svc := html[strings.Index(html, "Top services"):]
	if !strings.Contains(svc, "Compute Engine") || !strings.Contains(svc, `<tr class="tail">`) {
		t.Fatalf("services table: %s", svc)
	}
	if strings.Index(svc, "Feb 2026") > strings.Index(svc, "Mar 2026") || strings.Index(svc, "15.00") > strings.Index(svc, "19.00") {
		t.Error("prev month must come first in the services table too")
	}
}

func TestRenderErrorSection(t *testing.T) {
	s := Section{Display: "Broken", AccountID: "xxx", ReportLabel: "Mar 2026", PrevLabel: "Feb 2026",
		Error: "Table not found: foo.bar.baz", Services: []Line{{Name: "X"}}}
	html := mustRender(t, s)
	if !strings.Contains(html, "Failed to query this account: Table not found: foo.bar.baz") {
		t.Fatal("error text missing")
	}
	if strings.Contains(html, "Top services") || strings.Contains(html, "TOTAL") {
		t.Fatal("an error section shows no tables")
	}
	if _, err := Render("x", "p", nil); err == nil {
		t.Fatal("no sections must be an error")
	}
}

func TestRenderNoPercentage(t *testing.T) {
	s := testSection()
	s.Projects = []Line{{Key: "new", Name: "new", Report: 5, Delta: 5}}
	s.Total = Line{Report: 5, Delta: 5}
	if html := mustRender(t, s); !strings.Contains(html, "—</td>") {
		t.Fatal("a missing percentage renders as a dash")
	}
}

func TestRenderSubCentValues(t *testing.T) {
	s := testSection()
	s.Projects = []Line{{Name: "(unattributed)", Report: -0.002686, Prev: -0.000766, Delta: -0.00192}}
	s.Total = Line{Report: -0.002686, Prev: -0.000766, Delta: -0.00192}
	if html := mustRender(t, s); strings.Contains(html, "-0.00") {
		t.Fatal("a cell renders -0.00")
	}
	s.Projects = []Line{{Name: "tiny", Report: 0.003321, Prev: 0.0029, Delta: 0.000421}}
	s.Total = Line{Report: 0.003321, Prev: 0.0029, Delta: 0.000421}
	html := mustRender(t, s)
	if strings.Contains(html, "+0.00") || strings.Contains(html, `class="num delta-pos"`) || !strings.Contains(html, `class="num delta-zero"`) {
		t.Fatal("a change that shows as 0.00 is neither signed nor coloured")
	}
}

func TestFormatting(t *testing.T) {
	for in, want := range map[float64]string{-0.000766: "0.00", 0: "0.00", -0.006: "-0.01", -12.5: "-12.50", 3.14159: "3.14", 0.125: "0.12", 2.675: "2.67"} {
		if got := money(in); got != want {
			t.Errorf("money(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0.0004: "0.00", -0.0004: "0.00", 0: "0.00", 6.343429: "+6.34", -0.0075: "-0.01"} {
		if got := signedMoney(in); got != want {
			t.Errorf("signedMoney(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{0.0004: "delta-zero", -0.0004: "delta-zero", 0: "delta-zero", 6.34: "delta-pos", -6.34: "delta-neg"} {
		if got := deltaClass(in); got != want {
			t.Errorf("deltaClass(%v) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[float64]string{20.98: "+21%", -3.33: "-3%", 0: "0%", 0.5: "+0%", 2.5: "+2%"} {
		if got := pct(&in); got != want {
			t.Errorf("pct(%v) = %q, want %q", in, got, want)
		}
	}
	if pct(nil) != "—" {
		t.Error("pct(nil)")
	}
}
