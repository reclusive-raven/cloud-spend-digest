package gcp

import (
	"strings"
	"testing"
	"time"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
	"google.golang.org/api/bigquery/v2"
)

func TestMonthHelpers(t *testing.T) {
	if digest.MonthLabel("2026-08") != "Aug 2026" || digest.MonthBefore(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)) != "2025-12" {
		t.Fatal("month helpers")
	}
}

func TestQuerySQL(t *testing.T) {
	if strings.Contains(sqlTemplate, "ROUND(") {
		t.Error("per-group rounding biases the totals; round only when rendering")
	}
	if !strings.Contains(sqlTemplate, "invoice.month") || strings.Contains(sqlTemplate, "FORMAT_TIMESTAMP") {
		t.Error("filter on the invoice month, not usage time")
	}
	sql, err := querySQL("proj.billing_export.gcp_billing_export_v1_ABC_DEF_GHI")
	if err != nil || !strings.Contains(sql, "FROM `proj.billing_export.gcp_billing_export_v1_ABC_DEF_GHI`") {
		t.Fatalf("%v\n%s", err, sql)
	}
	for _, bad := range []string{"proj.billing_export.evil; DROP TABLE x", "a`b", "a b"} {
		if _, err := querySQL(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	p := queryParams("2026-04", "2026-03")
	if p[0].Name != "report_month" || p[0].ParameterValue.Value != "202604" || p[1].Name != "prev_month" || p[1].ParameterValue.Value != "202603" {
		t.Fatalf("params %+v %+v", p[0].ParameterValue, p[1].ParameterValue)
	}
}

func TestParseRows(t *testing.T) {
	schema := &bigquery.TableSchema{}
	for _, n := range []string{"project_id", "project_name", "service_id", "service_name", "month", "net_cost", "currency"} {
		schema.Fields = append(schema.Fields, &bigquery.TableFieldSchema{Name: n})
	}
	row := func(v ...any) *bigquery.TableRow {
		r := &bigquery.TableRow{}
		for _, x := range v {
			r.F = append(r.F, &bigquery.TableCell{V: x})
		}
		return r
	}
	rows, currency, err := parseRows(schema, []*bigquery.TableRow{
		row(nil, nil, "S", "Support", "2026-04", nil, "USD"),
		row("p", "Project", "S", "Service", "2026-04", "12.345", "USD"),
		row("q", "Q", "S", "Service", "2026-03", "1.5E-4", "USD"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if currency != "USD" || rows[0].ProjectID != "" || rows[0].NetCost != 0 || rows[1].NetCost != 12.345 || rows[2].NetCost != 0.00015 || rows[1].Month != "2026-04" {
		t.Fatalf("%q %+v", currency, rows)
	}
	if _, _, err := parseRows(&bigquery.TableSchema{}, []*bigquery.TableRow{row("p")}); err == nil {
		t.Fatal("a result missing its columns must be an error")
	}
}

func TestParseRowsRefusesUnknownAndMixedCurrencies(t *testing.T) {
	schema := &bigquery.TableSchema{}
	for _, n := range []string{"project_id", "project_name", "service_id", "service_name", "month", "net_cost", "currency"} {
		schema.Fields = append(schema.Fields, &bigquery.TableFieldSchema{Name: n})
	}
	row := func(currency string) *bigquery.TableRow {
		r := &bigquery.TableRow{}
		for _, v := range []string{"p", "P", "s", "S", "2026-09", "10", currency} {
			r.F = append(r.F, &bigquery.TableCell{V: v})
		}
		return r
	}
	for _, rows := range [][]*bigquery.TableRow{{row("")}, {row("USD"), row("LKR")}} {
		if _, _, err := parseRows(schema, rows); err == nil {
			t.Fatal("unqualified currency total accepted")
		}
	}
}

func TestParseRowsRefusesNonfiniteSpend(t *testing.T) {
	schema := &bigquery.TableSchema{}
	for _, name := range []string{"project_id", "project_name", "service_id", "service_name", "month", "net_cost", "currency"} {
		schema.Fields = append(schema.Fields, &bigquery.TableFieldSchema{Name: name})
	}
	for _, cost := range []string{"NaN", "Inf", "-Inf"} {
		row := &bigquery.TableRow{}
		for _, value := range []string{"p", "P", "s", "S", "2026-09", cost, "USD"} {
			row.F = append(row.F, &bigquery.TableCell{V: value})
		}
		if _, _, err := parseRows(schema, []*bigquery.TableRow{row}); err == nil || !strings.Contains(err.Error(), "subtotal unavailable") {
			t.Fatal("nonfinite spend must not enter budget arithmetic")
		}
	}
}
