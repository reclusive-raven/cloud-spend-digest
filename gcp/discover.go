package gcp

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"

	"google.golang.org/api/cloudbilling/v1"
	"google.golang.org/api/googleapi"
)

// notConfigured is an open account's section when none of its projects
// holds its export table, or the service account cannot read it yet.
const notConfigured = "BQ export unavailable (missing, not configured, or unreadable) for this billing account"

var accountName = regexp.MustCompile(`^billingAccounts/([A-Z0-9-]+)$`)

// account is one discovered or explicitly configured account and its export.
type account struct {
	ID      string
	Display string
	Mapped  bool // Explicit mappings bind queries to ID, including shared tables.
	// Table is a configured project.dataset.table, or empty for discovery.
	Table      string
	ModifiedAt time.Time
	Error      string
}

// discover snapshots the visible inventory without reading account exports.
func (a *API) discover(ctx context.Context, log *slog.Logger) ([]account, error) {
	var out []account
	err := a.billing.BillingAccounts.List().Pages(ctx, func(p *cloudbilling.ListBillingAccountsResponse) error {
		for _, ba := range p.BillingAccounts {
			if !ba.Open {
				continue
			}
			m := accountName.FindStringSubmatch(ba.Name)
			if m == nil {
				log.Warn("unexpected billing account name", "name", ba.Name)
				continue
			}
			acct := account{ID: m[1], Display: cmp.Or(ba.DisplayName, m[1])}
			out = append(out, acct)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("listing billing accounts: %w", safe(err))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// resolve finds or checks one account's export table.
func (a *API) resolve(ctx context.Context, log *slog.Logger, acct account) account {
	if acct.Table != "" {
		table := acct.Table
		acct.Table, acct.ModifiedAt, acct.Error = "", time.Time{}, ""
		if !ValidTable(table) {
			acct.Error = "Invalid configured export table"
			return acct
		}
		parts := strings.Split(table, ".")
		meta, err := a.bq.Tables.Get(parts[0], parts[1], parts[2]).Fields("id,lastModifiedTime").Context(ctx).Do()
		if err != nil {
			log.Warn("reading a configured export failed", "account", acct.ID, "err", safe(err))
			acct.Error = "Configured export unavailable (missing or unreadable); discovery fallback disabled"
			return acct
		}
		acct.Table = table
		if meta.LastModifiedTime > 0 {
			acct.ModifiedAt = time.UnixMilli(int64(meta.LastModifiedTime)).UTC()
		}
		return acct
	}

	projects, err := a.linkedProjects(ctx, "billingAccounts/"+acct.ID)
	if err != nil {
		log.Warn("listing a billing account's projects failed", "account", acct.ID, "err", safe(err))
		acct.Error = "Could not list linked projects; export evidence unavailable"
	} else {
		acct.Table, acct.ModifiedAt = a.probe(ctx, log, acct.ID, projects)
	}
	return acct
}

// linkedProjects lists the projects billed to an account. A failure part
// way through drops them all, as the old tool did.
func (a *API) linkedProjects(ctx context.Context, name string) ([]string, error) {
	var ids []string
	err := a.billing.BillingAccounts.Projects.List(name).Pages(ctx, func(r *cloudbilling.ListProjectBillingInfoResponse) error {
		for _, pi := range r.ProjectBillingInfo {
			if pi.BillingEnabled {
				ids = append(ids, pi.ProjectId)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// probe returns the first linked project's export table that exists.
func (a *API) probe(ctx context.Context, log *slog.Logger, accountID string, projects []string) (string, time.Time) {
	id := "gcp_billing_export_v1_" + strings.ReplaceAll(accountID, "-", "_")
	for _, p := range projects {
		path := p + ".billing_export." + id
		if !ValidTable(path) {
			continue
		}
		meta, err := a.bq.Tables.Get(p, "billing_export", id).Fields("id,lastModifiedTime").Context(ctx).Do()
		var gerr *googleapi.Error
		switch {
		case err == nil:
			var modified time.Time
			if meta.LastModifiedTime > 0 {
				modified = time.UnixMilli(int64(meta.LastModifiedTime)).UTC()
			}
			return path, modified
		case errors.As(err, &gerr) && gerr.Code == http.StatusNotFound:
		default:
			log.Warn("reading an export table failed", "table", path, "err", safe(err))
		}
	}
	return "", time.Time{}
}

// mappedAccounts overlays the mappings that apply to the report month on the
// discovered accounts.
func mappedAccounts(found []account, exports []Export, report string) []account {
	byID := map[string]account{}
	for _, a := range found {
		byID[a.ID] = a
	}
	for _, e := range exports {
		if !e.Applies(report) {
			continue
		}
		a := byID[e.AccountID]
		a.ID, a.Table, a.Mapped = e.AccountID, e.Table, true
		a.Display = cmp.Or(e.Display, a.Display, e.AccountID)
		byID[a.ID] = a
	}
	out := make([]account, 0, len(byID))
	for _, a := range byID {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
