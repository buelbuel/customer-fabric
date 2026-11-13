package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const sourceEvents = "/v1/source-events"

type eventReceipt struct {
	EventID        string `json:"event_id"`
	Outcome        string `json:"outcome"`
	ProfileID      string `json:"profile_id"`
	ProfileVersion *int64 `json:"profile_version"`
	MutationID     string `json:"mutation_id"`
}

type sourceRecord struct {
	profile, epoch, value string
}

// sourceRecord creates a record the way a source does, so creator authority is real.
func (s *consentSuite) sourceRecord(token string) sourceRecord {
	s.t.Helper()
	value := "record-" + platform.UUID()
	body := fmt.Sprintf(`{"set":{"display_name":"Synthetic"},"identifiers":[{"namespace":"crm","value":%q}]}`, value)
	w := s.callHeader("POST", "/v1/profiles", body, token, platform.UUID())
	s.expect(w, 201)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		s.t.Fatal(err)
	}
	r := sourceRecord{profile: created.ID, value: value}
	if err := s.pool.QueryRow(context.Background(), `SELECT record_epoch FROM external_identifiers WHERE value=$1`, value).Scan(&r.epoch); err != nil {
		s.t.Fatal(err)
	}
	return r
}

func eventBody(id string, r sourceRecord, epoch, sequence int64, change string) string {
	return fmt.Sprintf(`{"event_id":%q,"type":"record.updated.v1","record":{"namespace":"crm","value":%q},"source_epoch":%d,"sequence":%d,"occurred_at":"2026-01-01T00:00:00Z",%s}`,
		id, r.value, epoch, sequence, change)
}

func (s *consentSuite) sendEvent(token, body string) *httptest.ResponseRecorder {
	return s.call("POST", sourceEvents, body, token)
}

func (s *consentSuite) receipt(w *httptest.ResponseRecorder, status int, outcome string) eventReceipt {
	s.t.Helper()
	s.expect(w, status)
	var r eventReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil {
		s.t.Fatal(err)
	}
	if r.Outcome != outcome {
		s.t.Fatalf("outcome %s: %s", r.Outcome, w.Body.String())
	}
	return r
}

func (s *consentSuite) attribute(profile, name string) string {
	s.t.Helper()
	var value *string
	if err := s.pool.QueryRow(context.Background(), `SELECT attributes->>$2 FROM profiles WHERE id=$1`, profile, name).Scan(&value); err != nil {
		s.t.Fatal(err)
	}
	if value == nil {
		return ""
	}
	return *value
}

func (s *consentSuite) number(query string, args ...any) int64 {
	s.t.Helper()
	var n int64
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func (s *consentSuite) version(profile string) int64 {
	return s.number(`SELECT version FROM profiles WHERE id=$1`, profile)
}

func (s *consentSuite) outboxFor(profile string) int64 {
	return s.number(`SELECT count(*) FROM outbox WHERE profile_id=$1`, profile)
}

func TestEventUpdatesProfileWithProvenanceAndOutboxAtomically(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	got := s.receipt(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 10, `"set":{"tier":"gold"},"remove":["display_name"]`)), 201, "applied")
	if got.ProfileID != r.profile || got.ProfileVersion == nil || *got.ProfileVersion != 2 {
		t.Fatalf("receipt %+v", got)
	}
	if s.attribute(r.profile, "tier") != "gold" || s.attribute(r.profile, "display_name") != "" {
		t.Fatal("the event did not change the profile")
	}
	if n := s.number(`SELECT count(*) FROM provenance WHERE mutation_id=$1 AND source='importer' AND outcome='accepted'`, got.MutationID); n != 2 {
		t.Fatalf("provenance rows: %d", n)
	}
	var outboxType, payload string
	if err := s.pool.QueryRow(context.Background(), `SELECT type, payload::text FROM outbox WHERE id=$1`, got.MutationID).Scan(&outboxType, &payload); err != nil {
		t.Fatal(err)
	}
	if outboxType != "profile.updated.v1" || strings.Contains(payload, "gold") {
		t.Fatalf("outbox %s %s", outboxType, payload)
	}
	if n := s.number(`SELECT sequence FROM source_record_watermarks WHERE record_epoch=$1`, r.epoch); n != 10 {
		t.Fatalf("watermark %d", n)
	}
	if n := s.number(`SELECT count(*) FROM source_events e WHERE row_to_json(e)::text LIKE '%gold%'`); n != 0 {
		t.Fatalf("a receipt retained an attribute value: %d", n)
	}
}

func TestEventRetryReturnsOriginalReceipt(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	body := eventBody("evt-1", r, 1, 10, `"set":{"tier":"gold"}`)
	first := s.sendEvent(tokenB, body)
	s.expect(first, 201)
	s.receipt(s.sendEvent(tokenB, eventBody("evt-2", r, 1, 11, `"set":{"tier":"silver"}`)), 201, "applied")

	retry := s.sendEvent(tokenB, body)
	s.expect(retry, 200)
	if retry.Body.String() != first.Body.String() {
		t.Fatalf("retry %s of %s", retry.Body.String(), first.Body.String())
	}
	if s.attribute(r.profile, "tier") != "silver" || s.version(r.profile) != 3 || s.outboxFor(r.profile) != 3 {
		t.Fatal("a retry changed the profile or outbox")
	}
}

func TestEventIDCannotBeReusedForDifferentContent(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	s.expect(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 10, `"set":{"tier":"gold"}`)), 201)
	s.expect(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 10, `"set":{"tier":"forged"}`)), 409)
	s.expect(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 11, `"set":{"tier":"gold"}`)), 409)
	if s.attribute(r.profile, "tier") != "gold" {
		t.Fatal("a reused event ID changed the profile")
	}
	// An event ID is scoped to its source.
	other := s.sourceRecord(tokenA)
	s.receipt(s.sendEvent(tokenA, eventBody("evt-1", other, 1, 1, `"set":{"tier":"other"}`)), 201, "applied")
}

func TestStaleEventsAreReceiptedAndChangeNothing(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	s.expect(s.sendEvent(tokenB, eventBody("evt-10", r, 1, 10, `"set":{"tier":"gold"}`)), 201)
	version, outbox := s.version(r.profile), s.outboxFor(r.profile)

	older := eventBody("evt-9", r, 1, 9, `"set":{"tier":"bronze"}`)
	got := s.receipt(s.sendEvent(tokenB, older), 201, "stale")
	if got.ProfileVersion != nil || got.MutationID != "" {
		t.Fatalf("stale receipt %+v", got)
	}
	s.receipt(s.sendEvent(tokenB, eventBody("evt-10b", r, 1, 10, `"set":{"tier":"tied"}`)), 201, "stale")
	s.receipt(s.sendEvent(tokenB, older), 200, "stale")
	if s.attribute(r.profile, "tier") != "gold" || s.version(r.profile) != version || s.outboxFor(r.profile) != outbox {
		t.Fatal("a stale event changed the profile or outbox")
	}
	if n := s.number(`SELECT sequence FROM source_record_watermarks WHERE record_epoch=$1`, r.epoch); n != 10 {
		t.Fatalf("a stale event moved the watermark to %d", n)
	}
	// Ordering comes from the sequence alone. A later occurred_at does not make an old event current.
	future := strings.Replace(eventBody("evt-8", r, 1, 8, `"set":{"tier":"future"}`), "2026-01-01", "2099-01-01", 1)
	s.receipt(s.sendEvent(tokenB, future), 201, "stale")
}

func TestEventsAreScopedToTheCredentialsSource(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	body := eventBody("evt-1", r, 1, 1, `"set":{"tier":"forged"}`)
	s.expect(s.sendEvent(tokenA, body), 404)
	s.expect(s.sendEvent("", body), 401)
	s.expect(s.sendEvent(operatorToken, body), 401)
	s.expect(s.sendEvent(strings.Repeat("z", 64), body), 401)
	if s.attribute(r.profile, "tier") != "" || s.count("source_events") != 0 || s.count("source_record_watermarks") != 0 {
		t.Fatal("a rejected source left state behind")
	}
	s.expect(s.call("GET", sourceEvents, "", tokenB), 405)
	s.expect(s.call("POST", sourceEvents+"?source=console", body, tokenB), 400)
	unknown := eventBody("evt-2", sourceRecord{value: "no-such-record"}, 1, 1, `"set":{"a":"b"}`)
	s.expect(s.sendEvent(tokenB, unknown), 404)
}

// A source's events write only its own record. Another source's overlapping value for the same customer
// stays in that source's record, and both are kept (ADR 0018).
func TestEventsWriteOnlyTheSourcesOwnRecord(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	trustKey(t, s.h, operatorToken, "customer_number", "console", "importer")
	keyed := httptest.NewRequest("PATCH", "/v1/profiles/"+r.profile, strings.NewReader(`{"identifiers":[{"namespace":"customer_number","value":"C-5"}]}`))
	keyed.Header.Set("Content-Type", "application/json")
	keyed.Header.Set("Authorization", "Bearer "+tokenB)
	keyed.Header.Set("Idempotency-Key", platform.UUID())
	keyed.Header.Set("If-Match", fmt.Sprintf(`"%d"`, s.version(r.profile)))
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, keyed)
	s.expect(w, 200)
	create := httptest.NewRequest("POST", "/v1/profiles", strings.NewReader(`{"set":{"tier":"console-value"},"identifiers":[{"namespace":"customer_number","value":"C-5"}]}`))
	create.Header = keyed.Header.Clone()
	create.Header.Set("Authorization", "Bearer "+tokenA)
	create.Header.Set("Idempotency-Key", platform.UUID())
	create.Header.Del("If-Match")
	w = httptest.NewRecorder()
	s.h.ServeHTTP(w, create)
	s.expect(w, 201)
	var console profiles.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &console); err != nil {
		t.Fatal(err)
	}
	if _, err := (platform.Store{Pool: s.pool}).ReconcileIdentities(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := s.receipt(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 5, `"set":{"tier":"importer-value"}`)), 201, "applied")
	if got.ProfileID != r.profile || s.attribute(r.profile, "tier") != "importer-value" || s.attribute(console.ID, "tier") != "console-value" {
		t.Fatal("an event reached beyond the source's own record")
	}
	s.receipt(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 5, `"set":{"tier":"importer-value"}`)), 200, "applied")
	s.receipt(s.sendEvent(tokenB, eventBody("evt-0", r, 1, 4, `"set":{"own":"x"}`)), 201, "stale")
}

func TestEventDoesNotCrossAVerifiedAssociation(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	anchor, kratos := s.customerProfile()
	association := s.link(kratos, r.profile, r.epoch)
	before := s.version(anchor)

	w := s.sendEvent(tokenB, eventBody("evt-1", r, 1, 1, `"set":{"tier":"gold"}`))
	got := s.receipt(w, 201, "applied")
	if got.ProfileID != r.profile || s.version(anchor) != before || s.attribute(anchor, "tier") != "" {
		t.Fatal("an event crossed a verified association")
	}
	if s.number(`SELECT count(*) FROM verified_associations WHERE id=$1 AND revoked_at IS NULL AND record_epoch=$2`, association, r.epoch) != 1 {
		t.Fatal("an event changed the association")
	}
	for _, disclosure := range []string{kratos, anchor, association} {
		if strings.Contains(w.Body.String(), disclosure) {
			t.Fatal("the receipt disclosed the linked customer")
		}
	}
	// A source cannot aim at a customer anchor even if an identifier points to one.
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES('importer','crm','anchor-alias',$1)`, anchor); err != nil {
		t.Fatal(err)
	}
	s.expect(s.sendEvent(tokenB, eventBody("evt-2", sourceRecord{value: "anchor-alias"}, 1, 1, `"set":{"tier":"gold"}`)), 404)
	if s.attribute(anchor, "tier") != "" {
		t.Fatal("an event reached a customer anchor")
	}
}

func TestRecreatedRecordStartsANewEpoch(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	s.expect(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 100, `"set":{"tier":"gold"}`)), 201)
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `DELETE FROM external_identifiers WHERE record_epoch=$1`, r.epoch); err != nil {
		t.Fatal(err)
	}
	s.expect(s.sendEvent(tokenB, eventBody("evt-2", r, 1, 200, `"set":{"tier":"deleted"}`)), 404)
	var next string
	if err := s.pool.QueryRow(ctx, `INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES('importer','crm',$1,$2) RETURNING record_epoch`, r.value, r.profile).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next == r.epoch {
		t.Fatal("a recreated record kept its epoch")
	}
	s.receipt(s.sendEvent(tokenB, eventBody("evt-3", r, 1, 1, `"set":{"tier":"reborn"}`)), 201, "applied")
	if s.number(`SELECT sequence FROM source_record_watermarks WHERE record_epoch=$1`, r.epoch) != 100 ||
		s.number(`SELECT sequence FROM source_record_watermarks WHERE record_epoch=$1`, next) != 1 {
		t.Fatal("watermarks crossed record lifetimes")
	}
}

func TestSourceEpochFencesAndRestartsOrdering(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	applied := eventBody("evt-1", r, 1, 50, `"set":{"tier":"gold"}`)
	s.expect(s.sendEvent(tokenB, applied), 201)
	s.expect(s.sendEvent(tokenB, eventBody("evt-future", r, 2, 1, `"set":{"tier":"early"}`)), 409)

	advance := "/v1/operator/sources/importer/epoch"
	s.expect(s.advance(advance, tokenB, `"1"`), 401)
	s.expect(s.advance("/v1/operator/sources/no_such/epoch", operatorToken, `"1"`), 404)
	s.expect(s.advance(advance, operatorToken, ""), 428)
	s.expect(s.advance(advance, operatorToken, "1"), 400)
	s.expect(s.advance(advance, operatorToken, `"9"`), 412)
	s.expect(s.call("GET", advance, "", tokenB), 401)
	if read := s.call("GET", advance, "", operatorToken); read.Code != 200 || !strings.Contains(read.Body.String(), `"epoch":1`) {
		t.Fatalf("read %d %s", read.Code, read.Body.String())
	}
	w := s.advance(advance, operatorToken, `"1"`)
	s.expect(w, 200)
	// A retry after a lost response cannot advance it again.
	s.expect(s.advance(advance, operatorToken, `"1"`), 412)
	if !strings.Contains(w.Body.String(), `"epoch":2`) {
		t.Fatal(w.Body.String())
	}
	s.expect(s.sendEvent(tokenB, eventBody("evt-old", r, 1, 51, `"set":{"tier":"old"}`)), 409)
	s.receipt(s.sendEvent(tokenB, applied), 200, "applied")
	s.receipt(s.sendEvent(tokenB, eventBody("evt-new", r, 2, 1, `"set":{"tier":"new"}`)), 201, "applied")
	if s.attribute(r.profile, "tier") != "new" {
		t.Fatal("the new epoch did not restart ordering")
	}
	// Advancing one source leaves another alone.
	other := s.sourceRecord(tokenA)
	s.receipt(s.sendEvent(tokenA, eventBody("evt-a", other, 1, 1, `"set":{"tier":"x"}`)), 201, "applied")
}

func (s *consentSuite) advance(path, token, ifMatch string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", path, nil)
	r.Header.Set("Authorization", "Bearer "+token)
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

// An advance must get through while events keep arriving, and no old-epoch event may commit after it.
func TestEpochAdvanceIsNotStarvedByIngest(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	done := make(chan struct{})
	var wg sync.WaitGroup
	for writer := range 6 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for sequence := int64(1); ; sequence++ {
				select {
				case <-done:
					return
				default:
				}
				code := s.sendEvent(tokenB, eventBody(fmt.Sprintf("evt-%d-%d", writer, sequence), r, 1, sequence*10+int64(writer), `"set":{"tier":"x"}`)).Code
				if code != 201 && code != 409 {
					t.Errorf("status %d", code)
					return
				}
			}
		}()
	}
	w := s.advance("/v1/operator/sources/importer/epoch", operatorToken, `"1"`)
	close(done)
	wg.Wait()
	s.expect(w, 200)
}

func TestEventsForASecondOrderedRecordOfOneSourceAreRefused(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES('importer','crm','second-alias',$1)`, r.profile); err != nil {
		t.Fatal(err)
	}
	// The first record to receive an event takes the profile's event order for this source; the alias then
	// cannot be ordered with it, and its refused event leaves nothing behind.
	s.receipt(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 100, `"set":{"tier":"gold"}`)), 201, "applied")
	s.expect(s.sendEvent(tokenB, eventBody("evt-2", sourceRecord{value: "second-alias"}, 1, 1, `"set":{"tier":"bronze"}`)), 409)
	if s.attribute(r.profile, "tier") != "gold" || s.count("source_events") != 1 || s.count("source_record_watermarks") != 1 {
		t.Fatal("an ambiguous event left state behind")
	}
}

func TestLifecycleFenceRefusesEventsAndMutations(t *testing.T) {
	s := consentSetup(t)
	ctx := context.Background()
	for name, assignment := range map[string]string{
		"erased": "lifecycle_state='erased'", "erasure pending": "lifecycle_state='erasure_pending'", "restricted": "restricted=true",
	} {
		t.Run(name, func(t *testing.T) {
			r := s.sourceRecord(tokenB)
			applied := eventBody("evt-a-"+r.value, r, 1, 5, `"set":{"tier":"gold"}`)
			s.expect(s.sendEvent(tokenB, applied), 201)
			if _, err := s.pool.Exec(ctx, `UPDATE profiles SET `+assignment+` WHERE id=$1`, r.profile); err != nil {
				t.Fatal(err)
			}
			s.expect(s.sendEvent(tokenB, eventBody("evt-new-"+r.value, r, 1, 6, `"set":{"tier":"resurrected"}`)), 409)
			s.expect(s.sendEvent(tokenB, eventBody("evt-old-"+r.value, r, 1, 1, `"set":{"tier":"resurrected"}`)), 409)
			s.receipt(s.sendEvent(tokenB, applied), 200, "applied")
			if s.attribute(r.profile, "tier") != "gold" || s.number(`SELECT count(*) FROM source_events WHERE record_epoch=$1`, r.epoch) != 1 {
				t.Fatal("a fenced event left state behind")
			}
			patch := httptest.NewRequest("PATCH", "/v1/profiles/"+r.profile, strings.NewReader(`{"set":{"tier":"patched"}}`))
			patch.Header.Set("Content-Type", "application/json")
			patch.Header.Set("Authorization", "Bearer "+tokenB)
			patch.Header.Set("Idempotency-Key", platform.UUID())
			patch.Header.Set("If-Match", `"2"`)
			w := httptest.NewRecorder()
			s.h.ServeHTTP(w, patch)
			s.expect(w, 409)
		})
	}
}

func TestInvalidEventsAreRefusedBeforeAnyWrite(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	good := eventBody("evt-1", r, 1, 1, `"set":{"tier":"gold"}`)
	replace := func(a, b string) string { return strings.Replace(good, a, b, 1) }
	for name, tc := range map[string]struct {
		body   string
		status int
	}{
		"not json":         {"{", 400},
		"array":            {`[]`, 400},
		"unknown field":    {strings.TrimSuffix(good, "}") + `,"customer_subject":"x"}`, 400},
		"source field":     {strings.TrimSuffix(good, "}") + `,"source":"console"}`, 400},
		"identifiers":      {strings.TrimSuffix(good, "}") + `,"identifiers":[{"namespace":"crm","value":"x"}]}`, 400},
		"case alias":       {replace(`"set"`, `"SET"`), 400},
		"record alias":     {replace(`"namespace"`, `"Namespace"`), 400},
		"record field":     {replace(`"namespace"`, `"profile_id":"x","namespace"`), 400},
		"duplicate key":    {replace(`"sequence":1`, `"sequence":1,"sequence":2`), 400},
		"fractional":       {replace(`"sequence":1`, `"sequence":1.5`), 400},
		"string sequence":  {replace(`"sequence":1`, `"sequence":"1"`), 400},
		"bad time":         {replace(`2026-01-01T00:00:00Z`, `yesterday`), 400},
		"overflowing time": {replace(`2026-01-01T00:00:00Z`, `9999-12-31T23:59:59-01:00`), 422},
		"lone surrogate":   {replace(`"gold"`, `"\ud800"`), 422},
		"null set":         {replace(`"set":{"tier":"gold"}`, `"set":null`), 422},
		"null attribute":   {replace(`{"tier":"gold"}`, `{"tier":null}`), 422},
		"nested value":     {replace(`{"tier":"gold"}`, `{"tier":{"a":1}}`), 422},
		"no change":        {replace(`,"set":{"tier":"gold"}`, ``), 422},
		"bad event id":     {replace(`"evt-1"`, `"has space"`), 422},
		"unsupported":      {replace(`record.updated.v1`, `record.deleted.v1`), 422},
		"zero sequence":    {replace(`"sequence":1`, `"sequence":0`), 422},
		"zero epoch":       {replace(`"source_epoch":1`, `"source_epoch":0`), 422},
		"no sequence":      {replace(`"sequence":1,`, ``), 422},
	} {
		t.Run(name, func(t *testing.T) {
			s.expect(s.sendEvent(tokenB, tc.body), tc.status)
		})
	}
	if s.count("source_events") != 0 || s.count("source_record_watermarks") != 0 {
		t.Fatal("an invalid event left state behind")
	}
}

func TestProfileLimitRollsBackTheEvent(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	var set []string
	for i := range 100 {
		set = append(set, fmt.Sprintf(`"attribute_%d":"x"`, i))
	}
	s.expect(s.sendEvent(tokenB, eventBody("evt-1", r, 1, 1, `"set":{`+strings.Join(set, ",")+`}`)), 422)
	if s.count("source_events") != 0 || s.count("source_record_watermarks") != 0 || s.version(r.profile) != 1 {
		t.Fatal("a failed event left partial state")
	}
	if n := s.number(`SELECT count(*) FROM provenance WHERE profile_id=$1 AND path LIKE 'attribute_%'`, r.profile); n != 0 {
		t.Fatalf("provenance survived the rollback: %d", n)
	}
}

func TestConcurrentDuplicatesApplyOnce(t *testing.T) {
	s := consentSetup(t)
	r := s.sourceRecord(tokenB)
	body := eventBody("evt-1", r, 1, 1, `"set":{"tier":"gold"}`)
	const writers = 12
	codes := make([]int, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = s.sendEvent(tokenB, body).Code
		}()
	}
	wg.Wait()
	created := 0
	for _, code := range codes {
		if code == 201 {
			created++
		} else if code != 200 {
			t.Fatalf("status %d", code)
		}
	}
	if created != 1 || s.version(r.profile) != 2 || s.outboxFor(r.profile) != 2 {
		t.Fatalf("created %d, version %d, outbox %d", created, s.version(r.profile), s.outboxFor(r.profile))
	}
}

// Delivered in any order and concurrently, the highest sequence always decides the final value.
func TestConcurrentOutOfOrderEventsConvergeOnHighestSequence(t *testing.T) {
	s := consentSetup(t)
	for round := range 5 {
		r := s.sourceRecord(tokenB)
		const events = 16
		order := rand.Perm(events)
		var wg sync.WaitGroup
		for _, sequence := range order {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("evt-%d-%d", round, sequence)
				change := fmt.Sprintf(`"set":{"tier":"value-%d"}`, sequence)
				if code := s.sendEvent(tokenB, eventBody(id, r, 1, int64(sequence+1), change)).Code; code != 201 {
					t.Errorf("status %d", code)
				}
			}()
		}
		wg.Wait()
		if got := s.attribute(r.profile, "tier"); got != fmt.Sprintf("value-%d", events-1) {
			t.Fatalf("final value %s", got)
		}
		applied := s.number(`SELECT count(*) FROM source_events WHERE record_epoch=$1 AND outcome='applied'`, r.epoch)
		if s.version(r.profile) != applied+1 || s.outboxFor(r.profile) != applied+1 {
			t.Fatalf("version %d, outbox %d, applied %d", s.version(r.profile), s.outboxFor(r.profile), applied)
		}
		if s.number(`SELECT sequence FROM source_record_watermarks WHERE record_epoch=$1`, r.epoch) != events {
			t.Fatal("watermark did not reach the highest sequence")
		}
	}
}
