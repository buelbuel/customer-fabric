package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func (s linkingSuite) startImport(token, body string) platform.Import {
	s.t.Helper()
	res := s.request("POST", "/v1/imports", body, "", token, "")
	s.expect(res, 201)
	var imp platform.Import
	if err := json.Unmarshal(res.Body.Bytes(), &imp); err != nil {
		s.t.Fatal(err)
	}
	return imp
}

func (s linkingSuite) batch(token, id string, offset int64, rows ...string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/imports/"+id+"/rows", strings.NewReader(strings.Join(rows, "\n")+"\n"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/x-ndjson")
	req.Header.Set("Import-Offset", fmt.Sprint(offset))
	res := httptest.NewRecorder()
	s.h.ServeHTTP(res, req)
	return res
}

func progress(t *testing.T, res *httptest.ResponseRecorder) platform.Import {
	t.Helper()
	var imp platform.Import
	if err := json.Unmarshal(res.Body.Bytes(), &imp); err != nil {
		t.Fatalf("%d %s", res.Code, res.Body.String())
	}
	return imp
}

func row(value, set string) string {
	return fmt.Sprintf(`{"record":{"namespace":"crm","value":%q},"set":%s}`, value, set)
}

func TestImportValidatesQuarantinesAndIsolatesSources(t *testing.T) {
	s := setupLinking(t)
	s.expect(s.request("POST", "/v1/operator/attributes/tier", `{"type":"enum","values":["gold","silver"]}`, "", viewOperator, ""), 201)
	s.expect(s.request("POST", "/v1/operator/attributes/joined", `{"type":"date"}`, "", viewOperator, ""), 201)
	imp := s.startImport(tokenA, `{"max_errors":10}`)
	if imp.State != "open" || imp.Checkpoint != 0 || imp.MaxErrors != 10 {
		t.Fatalf("new import %+v", imp)
	}

	res := s.batch(tokenA, imp.ID, 1,
		row("imp-1", `{"tier":"gold","display_name":"Zoë 東京","joined":"2019-02-28"}`),
		row("imp-2", `{"tier":"silver"}`),
		`{"record":`,
		row("imp-3", `{"tier":"bronze"}`),
		row("imp-1", `{"tier":"silver"}`),
		`{"record":{"namespace":"crm","value":"imp-4"},"remove":["tier"]}`,
		row("imp-5", `{"joined":"2019-02-29"}`),
		`{"record":{"namespace":"crm","value":"imp-6"},"set":{"tier":"gold"},"profile_id":"x"}`,
	)
	s.expect(res, 200)
	got := progress(t, res)
	reasons := []string{}
	for _, q := range got.Quarantine {
		reasons = append(reasons, fmt.Sprintf("%d:%s", q.Line, q.Reason))
	}
	if got.Checkpoint != 8 || got.Created != 2 || got.Applied != 1 || got.Quarantined != 5 ||
		strings.Join(reasons, ",") != "3:invalid_json,4:invalid_attribute_value,6:invalid_row,7:invalid_attribute_value,8:invalid_row" {
		t.Fatalf("progress %+v", got)
	}
	if strings.Contains(res.Body.String(), "bronze") || strings.Contains(res.Body.String(), "2019-02-29") {
		t.Fatalf("progress echoed row content: %s", res.Body.String())
	}
	var tier, name string
	if err := s.pool.QueryRow(context.Background(), `SELECT p.attributes->>'tier', p.attributes->>'display_name' FROM profiles p
		JOIN external_identifiers e ON e.profile_id = p.id WHERE e.source='console' AND e.value='imp-1'`).Scan(&tier, &name); err != nil || tier != "silver" || name != "Zoë 東京" {
		t.Fatalf("imported record %q %q %v", tier, name, err)
	}

	// A resent or skipped batch is refused, so nothing is applied twice or lost.
	s.expect(s.batch(tokenA, imp.ID, 1, row("imp-1", `{"tier":"gold"}`)), 409)
	s.expect(s.batch(tokenA, imp.ID, 10, row("imp-9", `{"tier":"gold"}`)), 409)
	if p := progress(t, s.request("GET", "/v1/imports/"+imp.ID, "", "", tokenA, "")); p.Checkpoint != 8 || p.Applied != 1 {
		t.Fatalf("a refused batch changed progress %+v", p)
	}

	// Another source cannot see this import, and importing the same record value creates its own record.
	s.expect(s.request("GET", "/v1/imports/"+imp.ID, "", "", tokenB, ""), 404)
	s.expect(s.batch(tokenB, imp.ID, 9, row("imp-1", `{"tier":"gold"}`)), 404)
	other := s.startImport(tokenB, `{}`)
	s.expect(s.batch(tokenB, other.ID, 1, row("imp-1", `{"plan":"basic"}`)), 200)
	var profilesForValue int
	_ = s.pool.QueryRow(context.Background(), `SELECT count(DISTINCT profile_id) FROM external_identifiers WHERE value='imp-1'`).Scan(&profilesForValue)
	if profilesForValue != 2 {
		t.Fatalf("sources shared a record: %d profiles", profilesForValue)
	}

	// A record that already received a source event is never overwritten by historical rows.
	epoch, err := platform.Store{Pool: s.pool}.SourceEpoch(context.Background(), "console")
	if err != nil {
		t.Fatal(err)
	}
	event := fmt.Sprintf(`{"event_id":"imp-evt-1","type":"record.updated.v1","record":{"namespace":"crm","value":"imp-2"},"source_epoch":%d,"sequence":5,"occurred_at":"2026-11-01T00:00:00Z","set":{"tier":"gold"}}`, epoch)
	s.expect(s.request("POST", "/v1/source-events", event, "imp-evt-key-0000001", tokenA, ""), 201)
	got = progress(t, s.batch(tokenA, imp.ID, 9, row("imp-2", `{"tier":"silver"}`)))
	if got.Quarantine[len(got.Quarantine)-1].Reason != "newer_events_exist" {
		t.Fatalf("historical row overwrote an event: %+v", got.Quarantine)
	}

	completed := progress(t, s.request("POST", "/v1/imports/"+imp.ID+"/complete", "", "", tokenA, ""))
	if completed.State != "completed" {
		t.Fatalf("complete %+v", completed)
	}
	s.expect(s.batch(tokenA, imp.ID, 10, row("imp-9", `{"tier":"gold"}`)), 409)

	// Past the error budget the import fails and accepts nothing more.
	strict := s.startImport(tokenA, `{"max_errors":1}`)
	if p := progress(t, s.batch(tokenA, strict.ID, 1, `x`, `y`, row("imp-8", `{"tier":"gold"}`))); p.State != "failed" || p.Checkpoint != 2 {
		t.Fatalf("budget %+v", p)
	}
	s.expect(s.batch(tokenA, strict.ID, 3, row("imp-7", `{"tier":"gold"}`)), 409)

	for _, refused := range []struct {
		res    *httptest.ResponseRecorder
		status int
	}{
		{s.request("POST", "/v1/imports", `{}`, "", viewOperator, ""), 401},
		{s.customer("POST", "/v1/imports", `{}`, linkCustomerA), 401},
		{s.request("POST", "/v1/imports", `{"max_errors":-1}`, "", tokenA, ""), 422},
		{s.request("GET", "/v1/imports/"+platform.UUID(), "", "", tokenA, ""), 404},
		{s.request("POST", "/v1/imports/"+strict.ID+"/rows", row("a", `{}`), "", tokenA, ""), 415},
	} {
		s.expect(refused.res, refused.status)
	}
}

// 10,000 rows in batches, with a batch that fails part-way and a client that resumes from the checkpoint.
func TestImportResumesAfterMidBatchFailure(t *testing.T) {
	s := setupLinking(t)
	imp := s.startImport(tokenA, `{"max_errors":0}`)
	started := time.Now()
	rowsFor := func(from, to int) []string {
		out := []string{}
		for i := from; i <= to; i++ {
			out = append(out, row(fmt.Sprintf("bulk-%05d", i), fmt.Sprintf(`{"rank":%d}`, i)))
		}
		return out
	}
	// The third batch carries an oversized line at 2,500: rows before it commit, the rest do not.
	broken := rowsFor(2001, 3000)
	broken[499] = row("bulk-02500", `{"note":"`+strings.Repeat("x", 70<<10)+`"}`)
	s.expect(s.batch(tokenA, imp.ID, 1, rowsFor(1, 1000)...), 200)
	s.expect(s.batch(tokenA, imp.ID, 1001, rowsFor(1001, 2000)...), 200)
	s.expect(s.batch(tokenA, imp.ID, 2001, broken...), 413)
	resume := progress(t, s.request("GET", "/v1/imports/"+imp.ID, "", "", tokenA, ""))
	if resume.Checkpoint != 2499 || resume.State != "open" {
		t.Fatalf("after the failure %+v", resume)
	}
	for from := int(resume.Checkpoint) + 1; from <= 10000; from += 1000 {
		s.expect(s.batch(tokenA, imp.ID, int64(from), rowsFor(from, min(from+999, 10000))...), 200)
	}
	done := progress(t, s.request("GET", "/v1/imports/"+imp.ID, "", "", tokenA, ""))
	var records, provenance int
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM external_identifiers WHERE source='console' AND value LIKE 'bulk-%'`).Scan(&records)
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM provenance WHERE source='console' AND path='rank'`).Scan(&provenance)
	if done.Checkpoint != 10000 || done.Created != 10000 || done.Quarantined != 0 || records != 10000 || provenance != 10000 {
		t.Fatalf("10k import %+v records %d provenance %d", done, records, provenance)
	}
	t.Logf("imported 10,000 rows in %s", time.Since(started))
}
