package gcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
)

// fix is the setup an account needs before its export can be read.
const fix = `  To fix:
    1. Enable the Standard usage cost export for this billing account (Console:
       Billing > Billing export > BigQuery export), into a dataset named
       billing_export in a project linked to the same account. Google creates
       the table on its first write, which can take a day.
    2. Grant the identity running this tool READER on that billing_export dataset.
    3. Grant the same identity roles/billing.viewer on the billing account.
       Without it the account is invisible here, not reported as an error.
`

// Check lists each visible account's export and whether both months read,
// printing no figures, and explains what is missing. It returns an error when
// any account would leave the digest incomplete.
func (a *API) Check(ctx context.Context, out io.Writer, log *slog.Logger, report string, exports []Export) error {
	t, err := time.Parse("2006-01", report)
	if err != nil {
		return fmt.Errorf("month %q: want YYYY-MM", report)
	}
	prev := digest.MonthBefore(t)
	found, err := a.discover(ctx, log)
	if err != nil {
		return err
	}
	accounts := mappedAccounts(found, exports, report)
	if len(accounts) == 0 {
		fmt.Fprintf(out, "No open billing accounts are visible to this identity.\n%s", fix)
		return errors.New("no billing accounts visible")
	}
	failed := false
	for _, acct := range accounts {
		acct = a.resolve(ctx, log, acct)
		fmt.Fprintf(out, "%s (%s): ", acct.Display, acct.ID)
		if acct.Table == "" {
			failed = true
			fmt.Fprintf(out, "no readable export table.\n")
			if acct.Error != "" {
				fmt.Fprintf(out, "  %s\n", acct.Error)
			} else {
				fmt.Fprintf(out, "  None of its linked projects holds billing_export.gcp_billing_export_v1_<account id with underscores>, or this identity cannot read it.\n")
			}
			if !acct.Mapped {
				fmt.Fprint(out, fix)
			}
			continue
		}
		fmt.Fprintf(out, "%s\n", acct.Table)
		id := ""
		if acct.Mapped {
			id = acct.ID
		}
		req, err := accountQuery(acct.Table, report, prev, id)
		if err != nil {
			return err
		}
		req.DryRun = true
		res, err := a.bq.Jobs.Query(a.Project, req).Context(ctx).Do()
		if err == nil {
			err = jobError(res.Errors)
		}
		if err != nil {
			failed = true
			fmt.Fprintf(out, "  Query validation failed in project %s: %s\n  The identity needs roles/bigquery.jobUser there, and READER on the dataset.\n", a.Project, safe(err))
			continue
		}
		s := a.collectAccount(ctx, log, acct, report, prev)
		switch {
		case s.Coverage.NoComparison:
			fmt.Fprintf(out, "  Ready; the export began in %s, so there is no comparison with %s.\n", report, prev)
		case s.Complete():
			fmt.Fprintf(out, "  Ready; both %s and %s have rows.\n", prev, report)
		default:
			failed = true
			fmt.Fprintf(out, "  Not ready: %s\n", s.Coverage.Text())
		}
	}
	if failed {
		return errors.New("at least one account is not ready; a digest run would send nothing")
	}
	return nil
}
