# cloud-spend-digest

A run-once command that reads last month's cloud spend from your billing export and prints it as an HTML digest, or emails it. It keeps no state and needs no database, so a monthly scheduled job is all it takes to run.

Only Google Cloud is supported for now. For each open billing account the identity can see, the digest has a per-project table and the top ten services, with each figure set against the month before. Net spend is cost plus credits, grouped by invoice month.

## What it guarantees

- **No partial digests.** If any account's export is missing, unreadable, or has no rows for a month it should have, the command prints and sends nothing, names the account on stderr and exits 1. In a digest, a missing account would look the same as zero spend.
- **No invented comparisons.** If an export began in the report month, there is no earlier month to compare against. That account's previous-month and change columns show a dash, not `0.00`.
- **No figures in logs.** With `--to`, the digest goes only into the email. Stdout gets a single "sent" line, and error messages never carry amounts or remote response text.

Net spend is not your invoice total, and nothing is converted between currencies.

## Install

```sh
go install github.com/reclusive-raven/cloud-spend-digest@latest
```

## Usage

```sh
# Print last month's digest
cloud-spend-digest --project my-query-project > digest.html

# Email a given month
SMTP_USERNAME=me@example.com SMTP_PASSWORD=... \
  cloud-spend-digest --project my-query-project --month 2026-10 --to me@example.com,finance@example.com

# Explain what is missing before the first run
cloud-spend-digest check --project my-query-project
```

| Flag | Meaning |
|---|---|
| `--month YYYY-MM` | Report month. Defaults to the month before today. |
| `--project P` | Project that runs and pays for the BigQuery jobs. Defaults to `$GOOGLE_CLOUD_PROJECT`. It is required because federated and user credentials name no project of their own. |
| `--to a,b` | Email the digest to these addresses instead of printing it. |
| `--exports FILE` | Optional mappings, see below. |

Exit codes: 0 means done, 1 means an incomplete read or other failure (nothing was sent), and 2 means a usage error.

### Email

Mail goes out over SMTP using these environment variables:

| Variable | Default |
|---|---|
| `SMTP_HOST` | `smtp.gmail.com` |
| `SMTP_PORT` | `465` (implicit TLS; any other port must offer STARTTLS) |
| `SMTP_USERNAME` | required |
| `SMTP_PASSWORD` | required (for Gmail, an App Password) |
| `SMTP_FROM` | `SMTP_USERNAME` |

## Google Cloud setup

The tool signs in with [Application Default Credentials](https://cloud.google.com/docs/authentication/application-default-credentials). That covers a service account key in `GOOGLE_APPLICATION_CREDENTIALS`, `gcloud auth application-default login` (optionally with `--impersonate-service-account`), and Workload Identity Federation.

Each billing account needs:

1. **The standard usage cost export** to BigQuery (Billing → Billing export → BigQuery export → Standard usage cost). Write it to a dataset named `billing_export` in a project linked to that same billing account. Google creates `gcp_billing_export_v1_<ACCOUNT_ID with underscores>` on its first write, which can take a day. The export only runs forward and cannot be backfilled.
2. **READER on that `billing_export` dataset** for the identity running the tool.
3. **`roles/billing.viewer` on the billing account** for the same identity. Without it the account is invisible to the tool, and no error is reported.

The identity also needs `roles/bigquery.jobUser` on `--project`.

The tool discovers accounts on its own. It lists the open billing accounts the identity can see, then probes each linked project for the export table. `check` walks through the same steps and prints no figures. It tells you which of the three grants is missing.

### Mappings

Use `--exports` when discovery cannot find an export, for example an archive table shared by several accounts, or an account that has since been closed:

```json
[
  {"account_id": "AAAAAA-BBBBBB-CCCCCC", "display": "Main", "table": "archive-project.billing_export.gcp_billing_export_v1_AAAAAA_BBBBBB_CCCCCC",
   "from_month": "2026-01", "through_month": ""}
]
```

A mapped table is always queried with `billing_account_id` bound to the account, so a shared table only contributes that account's rows. The month bounds are optional and choose which report months the mapping applies to.

## Run it monthly from GitHub Actions

With Workload Identity Federation, the scheduled job needs no service account key:

```yaml
on:
  schedule:
    - cron: "30 2 2 * *"   # 02:30 UTC on the 2nd, a day after month end for export lag
  workflow_dispatch:
    inputs:
      month: { description: "YYYY-MM (default last month)", required: false }

permissions:
  contents: read
  id-token: write

jobs:
  digest:
    runs-on: ubuntu-24.04
    steps:
      - uses: actions/setup-go@v6
        with: { go-version: stable }
      - uses: google-github-actions/auth@v3
        with:
          workload_identity_provider: ${{ secrets.GCP_WIF_PROVIDER }}
          service_account: ${{ secrets.GCP_SERVICE_ACCOUNT }}
      - env:
          MONTH: ${{ inputs.month }}
          QUERY_PROJECT: ${{ secrets.QUERY_PROJECT }}
          DIGEST_TO: ${{ secrets.DIGEST_TO }}
          SMTP_USERNAME: ${{ secrets.SMTP_USERNAME }}
          SMTP_PASSWORD: ${{ secrets.SMTP_PASSWORD }}
        run: |
          go run github.com/reclusive-raven/cloud-spend-digest@v0.1.0 \
            --project "$QUERY_PROJECT" ${MONTH:+--month "$MONTH"} --to "$DIGEST_TO"
```

Keep the provider's attribute condition limited to the repository that runs the job, for example `assertion.repository_id == '<numeric id>'`.

## Adding another cloud

The `digest` package does the pivoting, the completeness checks and the rendering, and knows nothing about BigQuery. A new provider supplies `[]digest.Section`, one per billing account with its rows filled through `Section.Fill`, plus a `Check`. `gcp` is the reference implementation.

## Licence

MIT
