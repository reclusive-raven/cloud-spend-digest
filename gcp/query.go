package gcp

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
	"google.golang.org/api/bigquery/v2"
)

// sqlTemplate extends the original net-cost query with credit attribution.
// {table} is the only interpolation, guarded by ValidTable.
const sqlTemplate = `
SELECT
  project.id   AS project_id,
  project.name AS project_name,
  service.id   AS service_id,
  IFNULL(service.description, '(unknown service)') AS service_name,
  CONCAT(LEFT(invoice.month, 4), '-', RIGHT(invoice.month, 2)) AS month,
  -- Rounding is deferred to the renderer so the pivot can sum at full
  -- precision; per-group rounding here would accumulate ~$0.02 of bias when
  -- many small (project, service) groups roll up into a project total.
  SUM(cost) + SUM(IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0)) AS net_cost,
  currency,
  SUM(cost) AS gross_cost,
  SUM(IFNULL((SELECT SUM(c.amount) FROM UNNEST(credits) c), 0)) AS credits
FROM ` + "`{table}`" + `
-- Filter on invoice.month (Google's invoice-assignment column, YYYYMM)
-- rather than usage_start_time so the digest matches each month's actual
-- invoice. Rows with a NULL invoice.month (rare, unfinalized late usage)
-- fall through; they are picked up once Google assigns them a month.
WHERE invoice.month BETWEEN @prev_month AND @report_month
GROUP BY project_id, project_name, service_id, service_name, month, currency
ORDER BY month DESC, net_cost DESC
`

// querySQL fills the table name into the query.
func querySQL(table string) (string, error) {
	if !ValidTable(table) {
		return "", fmt.Errorf("untrusted table name: %q", table)
	}
	return strings.Replace(sqlTemplate, "{table}", table, 1), nil
}

// queryParams binds the months. The digest speaks YYYY-MM; invoice.month is
// YYYYMM, so the conversion happens here, at the boundary.
func queryParams(report, prev string) []*bigquery.QueryParameter {
	p := func(name, month string) *bigquery.QueryParameter {
		return &bigquery.QueryParameter{
			Name:           name,
			ParameterType:  &bigquery.QueryParameterType{Type: "STRING"},
			ParameterValue: &bigquery.QueryParameterValue{Value: strings.ReplaceAll(month, "-", "")},
		}
	}
	return []*bigquery.QueryParameter{p("report_month", report), p("prev_month", prev)}
}

// accountQuery preserves the digest calculation and binds an optional account
// scope for explicit mappings, which may point to a shared archive table.
func accountQuery(table, report, prev, accountID string) (*bigquery.QueryRequest, error) {
	sql, err := querySQL(table)
	if err != nil {
		return nil, err
	}
	params := queryParams(report, prev)
	if accountID != "" {
		sql = strings.Replace(sql, "WHERE invoice.month", "WHERE billing_account_id = @account_id AND invoice.month", 1)
		params = append(params, accountParam(accountID)...)
	}
	legacy := false
	return &bigquery.QueryRequest{Query: sql, UseLegacySql: &legacy, ParameterMode: "NAMED", QueryParameters: params, TimeoutMs: 60_000, JobCreationMode: "JOB_CREATION_REQUIRED"}, nil
}

// earliestMonth is the export's first invoice month as YYYY-MM, for this
// account alone when accountID binds a shared table. Empty when it has none.
func (a *API) earliestMonth(ctx context.Context, table, accountID string) (string, error) {
	if !ValidTable(table) {
		return "", fmt.Errorf("untrusted table name: %q", table)
	}
	sql := "SELECT MIN(invoice.month) AS earliest FROM `" + table + "`"
	var params []*bigquery.QueryParameter
	if accountID != "" {
		sql += " WHERE billing_account_id = @account_id"
		params = accountParam(accountID)
	}
	legacy := false
	res, err := a.bq.Jobs.Query(a.Project, &bigquery.QueryRequest{Query: sql, UseLegacySql: &legacy, ParameterMode: "NAMED", QueryParameters: params, TimeoutMs: 60_000}).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	if err := jobError(res.Errors); err != nil {
		return "", err
	}
	if !res.JobComplete {
		return "", errors.New("earliest-month query did not finish")
	}
	if res.Schema == nil || len(res.Schema.Fields) != 1 || res.Schema.Fields[0].Name != "earliest" {
		return "", errors.New("earliest-month query returned another shape")
	}
	if len(res.Rows) != 1 || len(res.Rows[0].F) != 1 {
		return "", nil
	}
	month, _ := res.Rows[0].F[0].V.(string)
	if len(month) != 6 {
		return "", nil
	}
	return month[:4] + "-" + month[4:], nil
}

func accountParam(accountID string) []*bigquery.QueryParameter {
	return []*bigquery.QueryParameter{{Name: "account_id", ParameterType: &bigquery.QueryParameterType{Type: "STRING"}, ParameterValue: &bigquery.QueryParameterValue{Value: accountID}}}
}

// query reads all pages of one export query and validates a single currency.
func (a *API) query(ctx context.Context, table, report, prev, accountID string) ([]digest.Row, string, error) {
	req, err := accountQuery(table, report, prev, accountID)
	if err != nil {
		return nil, "", err
	}
	return a.queryRows(ctx, req.Query, req.QueryParameters)
}

func (a *API) queryRows(ctx context.Context, sql string, params []*bigquery.QueryParameter) ([]digest.Row, string, error) {
	legacy := false
	res, err := a.bq.Jobs.Query(a.Project, &bigquery.QueryRequest{
		Query:           sql,
		UseLegacySql:    &legacy,
		ParameterMode:   "NAMED",
		QueryParameters: params,
		TimeoutMs:       60_000,
		// A job is needed to page through, or poll, the results.
		JobCreationMode: "JOB_CREATION_REQUIRED",
	}).Context(ctx).Do()
	if err != nil {
		return nil, "", err
	}
	if err := jobError(res.Errors); err != nil {
		return nil, "", err
	}
	schema, rows, complete, token := res.Schema, res.Rows, res.JobComplete, res.PageToken
	var all []*bigquery.TableRow
	for {
		all = append(all, rows...)
		if complete && token == "" {
			break
		}
		if res.JobReference == nil {
			return nil, "", errors.New("query did not finish and returned no job to wait on")
		}
		ref := res.JobReference
		call := a.bq.Jobs.GetQueryResults(ref.ProjectId, ref.JobId).Location(ref.Location).TimeoutMs(60_000)
		if complete {
			call = call.PageToken(token)
		}
		more, err := call.Context(ctx).Do()
		if err != nil {
			return nil, "", err
		}
		if err := jobError(more.Errors); err != nil {
			return nil, "", err
		}
		if more.Schema != nil {
			schema = more.Schema
		}
		rows, complete, token = more.Rows, more.JobComplete, more.PageToken
		if !complete {
			rows = nil
		}
	}
	return parseRows(schema, all)
}

func jobError(errs []*bigquery.ErrorProto) error {
	if len(errs) == 0 {
		return nil
	}
	reason := "unknown"
	switch errs[0].Reason {
	case "accessDenied", "invalidQuery", "notFound", "billingNotEnabled", "backendError", "rateLimitExceeded", "quotaExceeded":
		reason = errs[0].Reason
	}
	return fmt.Errorf("bigquery: job failed reason=%s (remote error text omitted)", reason)
}

// parseRows reads the result cells by column name. A NULL net_cost is 0.
func parseRows(schema *bigquery.TableSchema, rows []*bigquery.TableRow) ([]digest.Row, string, error) {
	col := map[string]int{}
	if schema != nil {
		for i, f := range schema.Fields {
			col[f.Name] = i
		}
	}
	for _, name := range []string{"project_id", "project_name", "service_id", "service_name", "month", "net_cost", "currency"} {
		if _, ok := col[name]; !ok && len(rows) > 0 {
			return nil, "", fmt.Errorf("result has no %s column", name)
		}
	}
	cell := func(r *bigquery.TableRow, name string) string {
		i, ok := col[name]
		if !ok {
			return ""
		}
		if i >= len(r.F) || r.F[i] == nil {
			return ""
		}
		s, _ := r.F[i].V.(string)
		return s
	}
	var out []digest.Row
	currency := ""
	for _, r := range rows {
		row := digest.Row{
			ProjectID: cell(r, "project_id"), ProjectName: cell(r, "project_name"),
			ServiceID: cell(r, "service_id"), ServiceName: cell(r, "service_name"),
			Month: cell(r, "month"),
		}
		if v := cell(r, "net_cost"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil {
				return nil, "", errors.New("invalid net_cost; subtotal unavailable")
			}
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, "", errors.New("export contains nonfinite net_cost; subtotal unavailable")
			}
			row.NetCost = f
		}
		rowCurrency := cell(r, "currency")
		if rowCurrency == "" {
			return nil, "", errors.New("export row has no currency; subtotal unavailable")
		}
		if currency != "" && currency != rowCurrency {
			return nil, "", errors.New("export contains mixed currencies; subtotal unavailable")
		}
		currency = rowCurrency
		out = append(out, row)
	}
	return out, currency, nil
}
