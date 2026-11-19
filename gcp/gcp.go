// Package gcp collects digest sections from Google Cloud billing exports in
// BigQuery, signed in with Application Default Credentials.
package gcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/bigquery/v2"
	"google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/googleapi"
	"google.golang.org/api/option"
)

// API holds the two Google clients a digest needs.
type API struct {
	// now defaults to wall time; tests supply a controlled clock.
	now     func() time.Time
	billing *cloudbilling.APIService
	bq      *bigquery.Service
	// Project runs and pays for the BigQuery jobs.
	Project string
}

// New signs in with Application Default Credentials. project is required:
// federated and user credentials name no project of their own.
func New(ctx context.Context, project string) (*API, error) {
	if project == "" {
		return nil, errors.New("no query project: pass --project or set GOOGLE_CLOUD_PROJECT")
	}
	creds, err := google.FindDefaultCredentials(ctx, bigquery.BigqueryScope, cloudbilling.CloudBillingReadonlyScope)
	if err != nil {
		return nil, fmt.Errorf("application default credentials: %w", err)
	}
	hc := oauth2.NewClient(ctx, creds.TokenSource)
	hc.Timeout = 90 * time.Second
	return newWith(ctx, hc, project)
}

// newWith builds the clients over hc. Extra endpoints are for tests, and
// apply in order: billing's first.
func newWith(ctx context.Context, hc *http.Client, project string, endpoints ...string) (*API, error) {
	bopts := []option.ClientOption{option.WithHTTPClient(hc)}
	qopts := []option.ClientOption{option.WithHTTPClient(hc)}
	if len(endpoints) == 2 {
		bopts = append(bopts, option.WithEndpoint(endpoints[0]))
		qopts = append(qopts, option.WithEndpoint(endpoints[1]))
	}
	billing, err := cloudbilling.NewService(ctx, bopts...)
	if err != nil {
		return nil, err
	}
	bq, err := bigquery.NewService(ctx, qopts...)
	if err != nil {
		return nil, err
	}
	return &API{billing: billing, bq: bq, Project: project}, nil
}

func (a *API) clock() time.Time {
	if a.now != nil {
		return a.now()
	}
	return time.Now()
}

// Export maps one billing account to a standard-schema export table, for an
// export discovery cannot find or a shared archive table. Optional bounds
// select report months, including their prior-month comparison.
type Export struct {
	AccountID    string `json:"account_id"`
	Display      string `json:"display"`
	Table        string `json:"table"`
	FromMonth    string `json:"from_month"`
	ThroughMonth string `json:"through_month"`
}

var (
	tableName     = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_]+\.[A-Za-z0-9_]+$`)
	accountIDForm = regexp.MustCompile(`^[A-Z0-9]{6}-[A-Z0-9]{6}-[A-Z0-9]{6}$`)
)

// ValidTable accepts a fully qualified project.dataset.table identifier.
// Keep interpolation narrow: no SQL, decorators, wildcards or quoted identifiers.
func ValidTable(table string) bool { return tableName.MatchString(table) }

// ValidateExports checks a mappings file before anything is queried.
func ValidateExports(exports []Export) error {
	var errs []error
	seen := map[string]bool{}
	for i, e := range exports {
		bad := func(message string) { errs = append(errs, fmt.Errorf("exports[%d]: %s", i, message)) }
		if !accountIDForm.MatchString(e.AccountID) {
			bad("account_id must be XXXXXX-XXXXXX-XXXXXX")
		}
		if seen[e.AccountID] {
			bad("duplicate account_id")
		}
		seen[e.AccountID] = true
		if !ValidTable(e.Table) {
			bad("table must be project.dataset.table")
		}
		for _, month := range []string{e.FromMonth, e.ThroughMonth} {
			if month != "" {
				if _, err := time.Parse("2006-01", month); err != nil {
					bad("month bounds must be YYYY-MM")
				}
			}
		}
		if e.FromMonth != "" && e.ThroughMonth != "" && e.FromMonth > e.ThroughMonth {
			bad("from_month must not follow through_month")
		}
	}
	return errors.Join(errs...)
}

// Applies reports whether the mapping is enabled for this report month.
func (e Export) Applies(report string) bool {
	return (e.FromMonth == "" || report >= e.FromMonth) && (e.ThroughMonth == "" || report <= e.ThroughMonth)
}

// safe drops remote response text, which can echo table names, principals or
// other private values, and keeps only the status.
func safe(err error) error {
	if err == nil {
		return nil
	}
	if remote, ok := errors.AsType[*googleapi.Error](err); ok {
		return fmt.Errorf("remote HTTP failure: status=%d (response omitted)", remote.Code)
	}
	if oauth, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
		status := 0
		if oauth.Response != nil {
			status = oauth.Response.StatusCode
		}
		return fmt.Errorf("OAuth failure: status=%d (response omitted)", status)
	}
	return err
}
