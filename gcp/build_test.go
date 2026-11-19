package gcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/reclusive-raven/cloud-spend-digest/digest"
)

// fakeGoogle serves the Cloud Billing and BigQuery calls a digest makes:
//
//   - AAAAAA: "Primary", linked to a project without the export and then
//     proj-a, which has it; its query is slow (polled) and paged.
//   - BBBBBB: no display name; proj-b has the export, the query is denied.
//   - CCCCCC: closed, so skipped.
//   - DDDDDD: "Fresh"; listing its projects fails, so not configured.
type fakeGoogle struct {
	mu      sync.Mutex
	queries []map[string]any
}

var fromTable = regexp.MustCompile("FROM `([^`]+)`")

func (f *fakeGoogle) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	send := func(w http.ResponseWriter, code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux.HandleFunc("GET /billing/v1/billingAccounts", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("pageToken") == "" {
			send(w, 200, map[string]any{"nextPageToken": "p2", "billingAccounts": []any{
				map[string]any{"name": "billingAccounts/DDDDDD-DDDDDD-DDDDDD", "displayName": "Fresh", "open": true},
				map[string]any{"name": "billingAccounts/AAAAAA-AAAAAA-AAAAAA", "displayName": "Primary", "open": true},
			}})
			return
		}
		send(w, 200, map[string]any{"billingAccounts": []any{
			map[string]any{"name": "billingAccounts/CCCCCC-CCCCCC-CCCCCC", "displayName": "Closed", "open": false},
			map[string]any{"name": "billingAccounts/BBBBBB-BBBBBB-BBBBBB", "open": true},
		}})
	})
	mux.HandleFunc("GET /billing/v1/billingAccounts/{id}/projects", func(w http.ResponseWriter, r *http.Request) {
		info := func(ids ...string) map[string]any {
			var out []any
			for _, id := range ids {
				out = append(out, map[string]any{"projectId": id, "billingEnabled": true})
			}
			out = append(out, map[string]any{"projectId": "unbilled", "billingEnabled": false})
			return map[string]any{"projectBillingInfo": out}
		}
		switch r.PathValue("id") {
		case "AAAAAA-AAAAAA-AAAAAA":
			send(w, 200, info("no-export", "proj-a"))
		case "BBBBBB-BBBBBB-BBBBBB":
			send(w, 200, info("proj-b"))
		default:
			send(w, 500, map[string]any{"error": map[string]any{"code": 500, "message": "backend error"}})
		}
	})
	mux.HandleFunc("GET /bq/projects/{p}/datasets/billing_export/tables/{t}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("p") + "/" + r.PathValue("t") {
		case "proj-a/gcp_billing_export_v1_AAAAAA_AAAAAA_AAAAAA", "proj-b/gcp_billing_export_v1_BBBBBB_BBBBBB_BBBBBB":
			send(w, 200, map[string]any{"id": r.PathValue("t"), "lastModifiedTime": "1790899200000"})
		default:
			send(w, 404, map[string]any{"error": map[string]any{"code": 404, "message": "Not found: Table"}})
		}
	})
	schema := map[string]any{"fields": []any{
		map[string]any{"name": "project_id"}, map[string]any{"name": "project_name"}, map[string]any{"name": "service_id"},
		map[string]any{"name": "service_name"}, map[string]any{"name": "month"}, map[string]any{"name": "net_cost"}, map[string]any{"name": "currency"},
	}}
	row := func(v ...any) map[string]any {
		var f []any
		for _, x := range v {
			f = append(f, map[string]any{"v": x})
		}
		return map[string]any{"f": f}
	}
	mux.HandleFunc("POST /bq/projects/{p}/queries", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		req["_project"] = r.PathValue("p")
		f.mu.Lock()
		f.queries = append(f.queries, req)
		f.mu.Unlock()
		m := fromTable.FindStringSubmatch(req["query"].(string))
		if m == nil {
			t.Errorf("no table in query")
			send(w, 400, nil)
			return
		}
		if strings.HasPrefix(m[1], "proj-b.") {
			send(w, 403, map[string]any{"error": map[string]any{"code": 403, "message": "Access Denied: Table " + m[1]}})
			return
		}
		send(w, 200, map[string]any{"jobComplete": false, "jobReference": map[string]any{"projectId": "host", "jobId": "job-a", "location": "US"}})
	})
	mux.HandleFunc("GET /bq/projects/{p}/queries/{job}", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("location") != "US" {
			t.Errorf("getQueryResults without the job's location")
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			send(w, 200, map[string]any{"jobComplete": true, "schema": schema, "pageToken": "page2", "rows": []any{
				row("proj-a", "Project A", "COMP", "Compute Engine", "2026-08", "12.5", "USD"),
				row(nil, nil, "INV", "Invoice", "2026-07", "-0.000579", "USD"),
			}})
		case "page2":
			send(w, 200, map[string]any{"jobComplete": true, "schema": schema, "rows": []any{
				row("proj-a", "Project A", "COMP", "Compute Engine", "2026-07", "10", "USD"),
				row(nil, nil, "INV", "Invoice", "2026-08", "0.003321", "USD"),
			}})
		default:
			t.Errorf("unexpected page token")
		}
	})
	return mux
}

func fakeAPI(t *testing.T) (*API, *fakeGoogle) {
	f := &fakeGoogle{}
	srv := httptest.NewServer(f.handler(t))
	t.Cleanup(srv.Close)
	a, err := newWith(context.Background(), srv.Client(), "host", srv.URL+"/billing/", srv.URL+"/bq/")
	if err != nil {
		t.Fatal(err)
	}
	return a, f
}

func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestCollect(t *testing.T) {
	a, f := fakeAPI(t)
	sections, err := a.Collect(context.Background(), quiet(), "2026-08", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(sections) != 3 {
		t.Fatalf("%d sections", len(sections))
	}
	primary, denied, fresh := sections[0], sections[1], sections[2]
	if primary.AccountID != "AAAAAA-AAAAAA-AAAAAA" || primary.Display != "Primary" || primary.Error != "" || primary.Currency != "USD" || !primary.Complete() {
		t.Fatalf("primary %+v", primary)
	}
	if primary.Coverage.UpdatedAt.IsZero() {
		t.Fatal("export modification evidence lost")
	}
	if len(primary.Projects) != 2 || primary.Projects[0].Name != "Project A" || !near(primary.Projects[0].Delta, 2.5) || primary.Projects[1].Name != "(unattributed)" {
		t.Fatalf("primary projects %+v", primary.Projects)
	}
	if primary.Projects[1].Pct != nil || primary.Total.Pct == nil {
		t.Fatal("a negative baseline carries no percentage; the total's positive one does")
	}
	if denied.Display != "BBBBBB-BBBBBB-BBBBBB" || !strings.Contains(denied.Error, "status=403") || denied.Complete() {
		t.Fatalf("denied %+v", denied)
	}
	if fresh.Display != "Fresh" || fresh.Error != "Could not list linked projects; export evidence unavailable" || fresh.Complete() {
		t.Fatalf("fresh %+v", fresh)
	}
	if strings.Contains(denied.Error, "proj-b") {
		t.Fatal("remote error text leaked into the section")
	}
	if len(f.queries) != 2 {
		t.Fatalf("%d queries, want one per found export", len(f.queries))
	}
	q := f.queries[0]
	params, _ := json.Marshal(q["queryParameters"])
	if q["_project"] != "host" || q["useLegacySql"] != false || q["parameterMode"] != "NAMED" || q["jobCreationMode"] != "JOB_CREATION_REQUIRED" ||
		!strings.Contains(string(params), `"value":"202608"`) || !strings.Contains(string(params), `"value":"202607"`) {
		t.Fatalf("query request %v", q)
	}
	html, err := digest.Render("GCP spend — Aug 2026", "BigQuery jobs run in host.", sections[:1])
	if err != nil || !strings.Contains(html, "Primary (AAAAAA-AAAAAA-AAAAAA) &mdash; USD") || !strings.Contains(html, "Top services") {
		t.Fatalf("%v\n%s", err, html)
	}
}

func TestEmptyExportsAreIncomplete(t *testing.T) {
	f := &fakeGoogle{}
	base := f.handler(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/queries") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"jobComplete":true,"rows":[]}`)
			return
		}
		base.ServeHTTP(w, r)
	}))
	defer srv.Close()
	a, err := newWith(context.Background(), srv.Client(), "host", srv.URL+"/billing/", srv.URL+"/bq/")
	if err != nil {
		t.Fatal(err)
	}
	sections, err := a.Collect(context.Background(), quiet(), "2026-09", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range sections {
		if s.Complete() || len(s.Projects) != 0 {
			t.Fatalf("an empty export is missing evidence, not zero spend: %+v", s)
		}
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }
