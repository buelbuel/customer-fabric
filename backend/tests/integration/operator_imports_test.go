package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// sourceImportBatch posts NDJSON rows for a source import through h.
func sourceImportBatch(h http.Handler, token, id string, offset int64, rows ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/imports/"+id+"/rows", strings.NewReader(strings.Join(rows, "\n")+"\n"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Import-Offset", fmt.Sprint(offset))
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

// Operators reconcile source imports from checkpoints and quarantine reason codes. The view spans
// sources, needs source_admin, and never returns row content or the CSV mapping.
func TestOperatorReadsImportReconciliation(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "source_admin")
	h := s.kratosAPI(issuer.url)

	start := func(token, body string) platform.Import {
		t.Helper()
		res := s.call("POST", "/v1/imports", body, token)
		if res.Code != 201 {
			t.Fatalf("start import: %d %s", res.Code, res.Body.String())
		}
		var imp platform.Import
		if err := json.Unmarshal(res.Body.Bytes(), &imp); err != nil {
			t.Fatal(err)
		}
		return imp
	}
	const marker = "synthetic-reconcile-marker"
	open := start(tokenA, `{"max_errors":10}`)
	res := sourceImportBatch(h, tokenA, open.ID, 1,
		`{"record":{"namespace":"crm","value":"`+marker+`-1"},"set":{"display_name":"`+marker+`"}}`,
		`{"record":`,
		`{"record":{"namespace":"crm","value":"`+marker+`-2"},"set":{},"profile_id":"x"}`,
		`not json `+marker,
	)
	if res.Code != 200 {
		t.Fatalf("batch: %d %s", res.Code, res.Body.String())
	}
	csv := start(tokenB, `{"csv":{"namespace":"erp","record_column":"SYNTH_COLUMN_MARKER","columns":{"SYNTH_TIER_MARKER":"tier"}}}`)
	if w := s.call("POST", "/v1/imports/"+csv.ID+"/complete", "", tokenB); w.Code != 200 {
		t.Fatalf("complete: %d %s", w.Code, w.Body.String())
	}

	// Authentication and role boundaries.
	if w := s.call("GET", "/v1/operator/imports", "", ""); w.Code != 401 {
		t.Fatalf("anonymous list: %d", w.Code)
	}
	if w := s.call("GET", "/v1/operator/imports", "", tokenA); w.Code != 401 {
		t.Fatalf("source credential listed imports: %d", w.Code)
	}
	if w := s.call("GET", "/v1/operator/imports/"+open.ID, "", tokenA); w.Code != 401 {
		t.Fatalf("source credential read an import as operator: %d", w.Code)
	}
	for _, roles := range [][]string{{"profile_reader"}, {"audit_reader"}, {"privacy_admin", "consent_admin"}} {
		issuer.assign(roles...)
		for _, path := range []string{"/v1/operator/imports", "/v1/operator/imports/" + open.ID} {
			if w := operatorSession(h, "GET", path, ""); w.Code != 403 || !strings.Contains(w.Body.String(), "source_admin") {
				t.Fatalf("%v read %s: %d %s", roles, path, w.Code, w.Body.String())
			}
		}
	}
	issuer.assign("source_admin")

	// Read-only, bounded resource.
	for _, c := range []struct {
		method, path string
		status       int
	}{
		{"POST", "/v1/operator/imports", 405},
		{"DELETE", "/v1/operator/imports/" + open.ID, 405},
		{"GET", "/v1/operator/imports?source=console", 400},
		{"GET", "/v1/operator/imports/" + open.ID + "?limit=5", 400},
		{"GET", "/v1/operator/imports/not-an-id", 404},
		{"GET", "/v1/operator/imports/" + platform.UUID(), 404},
		{"GET", "/v1/operator/imports/" + open.ID + "/rows", 404},
	} {
		if w := operatorSession(h, c.method, c.path, ""); w.Code != c.status {
			t.Fatalf("%s %s: %d, want %d: %s", c.method, c.path, w.Code, c.status, w.Body.String())
		}
	}

	list := operatorSession(h, "GET", "/v1/operator/imports", "")
	if list.Code != 200 {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	for _, leaked := range []string{marker, "SYNTH_COLUMN_MARKER", "SYNTH_TIER_MARKER", "mapping", "erp"} {
		if strings.Contains(list.Body.String(), leaked) {
			t.Fatalf("list disclosed %q: %s", leaked, list.Body.String())
		}
	}
	var listed struct {
		Items []platform.ImportReconciliation `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Items) != 2 || listed.Items[0].ID != csv.ID || listed.Items[1].ID != open.ID {
		t.Fatalf("most recently updated first, across sources: %s", list.Body.String())
	}
	done, got := listed.Items[0], listed.Items[1]
	if done.Source != "importer" || done.Format != "csv" || done.State != "completed" || done.ResumeAt != nil {
		t.Fatalf("completed import %+v", done)
	}
	if got.Source != "console" || got.Format != "ndjson" || got.State != "open" || got.Checkpoint != 4 ||
		got.ResumeAt == nil || *got.ResumeAt != 5 || got.Created != 1 || got.Quarantined != 3 || got.MaxErrors != 10 {
		t.Fatalf("open import %+v", got)
	}

	detail := operatorSession(h, "GET", "/v1/operator/imports/"+open.ID, "")
	if detail.Code != 200 || strings.Contains(detail.Body.String(), marker) {
		t.Fatalf("detail: %d %s", detail.Code, detail.Body.String())
	}
	var one platform.ImportReconciliationDetail
	if err := json.Unmarshal(detail.Body.Bytes(), &one); err != nil {
		t.Fatal(err)
	}
	reasons := []string{}
	for _, r := range one.QuarantineReasons {
		reasons = append(reasons, fmt.Sprintf("%s=%d", r.Reason, r.Count))
	}
	lines := []string{}
	for _, q := range one.Quarantine {
		lines = append(lines, fmt.Sprintf("%d:%s", q.Line, q.Reason))
	}
	if one.ID != open.ID || one.Source != "console" || strings.Join(reasons, ",") != "invalid_json=2,invalid_row=1" ||
		strings.Join(lines, ",") != "2:invalid_json,3:invalid_row,4:invalid_json" {
		t.Fatalf("detail %+v lines %v", one, lines)
	}
	// Another source's import is visible to the operator, but its detail still carries no mapping.
	other := operatorSession(h, "GET", "/v1/operator/imports/"+csv.ID, "")
	if other.Code != 200 || strings.Contains(other.Body.String(), "SYNTH_COLUMN_MARKER") || !strings.Contains(other.Body.String(), `"quarantine":[]`) {
		t.Fatalf("csv detail: %d %s", other.Code, other.Body.String())
	}
	// Reads are not operator writes, so they leave no audit entry.
	if n := s.count("operator_audit"); n != 0 {
		t.Fatalf("reads were audited as writes: %d", n)
	}
}
