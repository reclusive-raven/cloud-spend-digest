package gcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
)

const newExportTable = "synthetic.billing_export.gcp_billing_export_v1_AAAAAA_AAAAAA_AAAAAA"

// newExportFake answers the digest query with rows and the earliest-month
// query with earliest. An empty earliest is a refused query. It counts the
// earliest-month queries and keeps the last one's request.
type newExportFake struct {
	rows     [][]any
	earliest string
	asked    int
	last     map[string]any
}

func (f *newExportFake) api(t *testing.T) *API {
	t.Helper()
	cell := func(v []any) map[string]any {
		var out []any
		for _, x := range v {
			out = append(out, map[string]any{"v": x})
		}
		return map[string]any{"f": out}
	}
	fields := func(names ...string) map[string]any {
		var out []any
		for _, n := range names {
			out = append(out, map[string]any{"name": n})
		}
		return map[string]any{"fields": out}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(req["query"].(string), "MIN(invoice.month)") {
			f.asked++
			f.last = req
			if f.earliest == "" {
				w.WriteHeader(http.StatusForbidden)
				_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": 403, "message": "denied"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"jobComplete": true, "schema": fields("earliest"), "rows": []any{cell([]any{f.earliest})}})
			return
		}
		var rows []any
		for _, row := range f.rows {
			rows = append(rows, cell(row))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jobComplete": true, "schema": fields("project_id", "project_name", "service_id", "service_name", "month", "net_cost", "currency"), "rows": rows})
	}))
	t.Cleanup(srv.Close)
	a, err := newWith(context.Background(), srv.Client(), "host", srv.URL+"/billing/", srv.URL+"/bq/")
	if err != nil {
		t.Fatal(err)
	}
	a.now = func() time.Time { return time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC) }
	return a
}

var (
	octoberRow   = []any{"proj", "Project", "COMP", "Compute Engine", "2026-10", "0.1044", "USD"}
	septemberRow = []any{"proj", "Project", "COMP", "Compute Engine", "2026-09", "0.09", "USD"}
)

func collectNew(t *testing.T, f *newExportFake, acct account) digest.Section {
	t.Helper()
	if acct.Table == "" {
		acct.Table = newExportTable
	}
	return f.api(t).collectAccount(context.Background(), quiet(), acct, "2026-10", "2026-09")
}

// An export that began in the report month can never show the month before,
// so holding for it would hold every later report too. It is collected, and
// says there is no comparison rather than showing a zero.
func TestExportBeginningInReportMonthHasNoComparison(t *testing.T) {
	f := &newExportFake{rows: [][]any{octoberRow}, earliest: "202610"}
	s := collectNew(t, f, account{ID: "AAAAAA-AAAAAA-AAAAAA", Display: "New"})
	if !s.Complete() || !s.Coverage.NoComparison || s.Coverage.Completeness != "unverified" {
		t.Fatalf("%+v", s.Coverage)
	}
	if f.asked != 1 {
		t.Fatalf("%d earliest-month queries", f.asked)
	}
	if got := strings.Join(s.Coverage.Details, " "); !strings.Contains(got, "No rows for 2026-09") || !strings.Contains(got, "not confirmed zero spend") {
		t.Fatal(got)
	}
	if !s.Total.NoComparison || !s.Projects[0].NoComparison || !s.Services[0].NoComparison {
		t.Fatalf("a line kept its comparison: %+v", s)
	}
	html, err := digest.Render("GCP spend", "", []digest.Section{s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(html, `<td class="num">0.00</td>`) || strings.Contains(html, "+0.10") {
		t.Fatalf("a missing month reads as zero spend:\n%s", html)
	}
	if !strings.Contains(html, `<td class="num">—</td>`) || !strings.Contains(html, `<td class="num">0.10</td>`) {
		t.Fatalf("no dash beside the report month:\n%s", html)
	}
}

func TestMissingComparisonStillHoldsWithHistory(t *testing.T) {
	for name, earliest := range map[string]string{"older rows": "202608", "refused query": ""} {
		t.Run(name, func(t *testing.T) {
			f := &newExportFake{rows: [][]any{octoberRow}, earliest: earliest}
			s := collectNew(t, f, account{ID: "AAAAAA-AAAAAA-AAAAAA", Display: "Old"})
			if s.Complete() || s.Coverage.NoComparison || s.Coverage.Completeness != "partial" || s.Total.NoComparison {
				t.Fatalf("%+v", s.Coverage)
			}
			if !strings.Contains(s.Coverage.Text(), "No rows for 2026-09") {
				t.Fatal(s.Coverage.Text())
			}
		})
	}
}

func TestEarliestMonthIsReadOnlyForAMissingComparison(t *testing.T) {
	for name, rows := range map[string][][]any{
		"both months":     {octoberRow, septemberRow},
		"no report month": {septemberRow},
		"neither month":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			f := &newExportFake{rows: rows, earliest: "202610"}
			s := collectNew(t, f, account{ID: "AAAAAA-AAAAAA-AAAAAA", Display: "Old"})
			if f.asked != 0 || s.Coverage.NoComparison {
				t.Fatalf("asked %d, %+v", f.asked, s.Coverage)
			}
		})
	}
}

// A shared archive table holds other accounts' months, which prove nothing
// about this account's history.
func TestEarliestMonthBindsAMappedAccount(t *testing.T) {
	f := &newExportFake{rows: [][]any{octoberRow}, earliest: "202610"}
	s := collectNew(t, f, account{ID: "AAAAAA-AAAAAA-AAAAAA", Display: "Mapped", Mapped: true})
	if !s.Coverage.NoComparison {
		t.Fatalf("%+v", s.Coverage)
	}
	if !strings.Contains(f.last["query"].(string), "billing_account_id = @account_id") {
		t.Fatal(f.last["query"])
	}
	params, _ := json.Marshal(f.last["queryParameters"])
	if !strings.Contains(string(params), "AAAAAA-AAAAAA-AAAAAA") {
		t.Fatal(string(params))
	}
}
