package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const attributes = "/v1/operator/attributes"

func (s *consentSuite) define(path, body, ifMatch string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", attributes+"/"+path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer "+operatorToken)
	if ifMatch != "" {
		r.Header.Set("If-Match", ifMatch)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

// revisionOf returns the definition revision recorded for an accepted write, or 0 when untyped.
func (s *consentSuite) revisionOf(mutation, path string) int64 {
	return s.number(`SELECT coalesce(attribute_revision, 0) FROM provenance WHERE mutation_id=$1 AND path=$2`, mutation, path)
}

func TestAttributeDefinitionsAreOperatorManaged(t *testing.T) {
	s := consentSetup(t)
	s.expect(s.call("GET", attributes, "", ""), 401)
	s.expect(s.call("GET", attributes, "", tokenB), 401)
	s.expect(s.call("POST", attributes+"/tier", `{"type":"enum","values":["gold"]}`, tokenB), 401)
	s.expect(s.call("POST", attributes, `{"type":"boolean"}`, operatorToken), 405)
	s.expect(s.call("GET", attributes+"/Not-A-Name", "", operatorToken), 404)
	s.expect(s.call("GET", attributes+"/tier", "", operatorToken), 404)
	s.expect(s.define("tier", `{"type":"enum"}`, ""), 422)
	s.expect(s.define("tier", `{"type":"enum","values":["gold"],"extra":1}`, ""), 400)

	created := s.define("tier", `{"type":"enum","values":["gold","silver"]}`, "")
	s.expect(created, 201)
	if created.Header().Get("ETag") != `"1"` {
		t.Fatalf("etag %q", created.Header().Get("ETag"))
	}
	s.expect(s.define("tier", `{"type":"enum","values":["gold","silver","bronze"]}`, ""), 428)
	s.expect(s.define("tier", `{"type":"enum","values":["gold","silver","bronze"]}`, `"2"`), 412)
	s.expect(s.define("nickname", `{"type":"string","values":[]}`, ""), 422)
	s.expect(s.define("tier", `{"type":"enum","values":["gold"]}`, `"1"`), 409)
	s.expect(s.define("tier", `{"type":"string"}`, `"1"`), 409)
	s.expect(s.define("tier", `{"type":"enum","values":["gold","silver","bronze"]}`, `1`), 400)
	s.expect(s.define("tier", `{"type":"enum","values":["gold","silver","bronze"]}`, `"1"`), 201)
	s.expect(s.define("orders", `{"type":"integer"}`, ""), 201)

	list := s.call("GET", attributes, "", operatorToken)
	s.expect(list, 200)
	var current struct {
		Items []struct {
			Path     string   `json:"path"`
			Revision int32    `json:"revision"`
			Values   []string `json:"values"`
		} `json:"items"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if len(current.Items) != 2 || current.Items[0].Path != "orders" || current.Items[1].Revision != 2 || len(current.Items[1].Values) != 3 {
		t.Fatalf("current %s", list.Body.String())
	}
	history := s.call("GET", attributes+"/tier", "", operatorToken)
	s.expect(history, 200)
	if strings.Count(history.Body.String(), `"revision"`) != 2 {
		t.Fatalf("history %s", history.Body.String())
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE attribute_definitions SET enum_values='{gold}'`); err == nil {
		t.Fatal("a revision was edited in place")
	}
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM attribute_definitions`); err == nil {
		t.Fatal("a revision was deleted")
	}
}

// The Epic 1 scenario: two source records receive typed values; a valid update, a retry, a stale
// event and an invalid value each have a deterministic outcome.
func TestTypedAttributesGovernSourceEvents(t *testing.T) {
	s := consentSetup(t)
	s.expect(s.define("tier", `{"type":"enum","values":["gold","silver"]}`, ""), 201)
	s.expect(s.define("lifetime_value", `{"type":"decimal"}`, ""), 201)
	s.expect(s.define("signup_date", `{"type":"date"}`, ""), 201)
	crm := s.sourceRecord(tokenB)
	shop := s.sourceRecord(tokenA)

	first := eventBody("crm-1", crm, 1, 1, `"set":{"tier":"silver","signup_date":"2016-03-01","lifetime_value":120.5}`)
	applied := s.receipt(s.sendEvent(tokenB, first), 201, "applied")
	if s.revisionOf(applied.MutationID, "tier") != 1 || s.revisionOf(applied.MutationID, "signup_date") != 1 {
		t.Fatal("provenance does not record the definition revision")
	}
	s.receipt(s.sendEvent(tokenA, eventBody("shop-1", shop, 1, 1, `"set":{"tier":"gold","lifetime_value":80}`)), 201, "applied")

	update := s.receipt(s.sendEvent(tokenB, eventBody("crm-2", crm, 1, 2, `"set":{"tier":"gold"}`)), 201, "applied")
	retry := s.sendEvent(tokenB, eventBody("crm-2", crm, 1, 2, `"set":{"tier":"gold"}`))
	s.expect(retry, 200)
	if again := s.receipt(retry, 200, "applied"); again.MutationID != update.MutationID {
		t.Fatal("a retry produced a new mutation")
	}
	s.receipt(s.sendEvent(tokenB, eventBody("crm-0", crm, 1, 1, `"set":{"tier":"silver"}`)), 201, "stale")
	if s.attribute(crm.profile, "tier") != "gold" {
		t.Fatal("a stale event changed the value")
	}

	outbox := s.outboxFor(crm.profile)
	for _, change := range []string{`"set":{"tier":"platinum"}`, `"set":{"signup_date":"2016-02-30"}`, `"set":{"lifetime_value":"120.50"}`} {
		w := s.sendEvent(tokenB, eventBody("crm-bad-"+platform.UUID(), crm, 1, 3, change))
		s.expect(w, 422)
		if strings.Contains(w.Body.String(), "platinum") || strings.Contains(w.Body.String(), "2016-02-30") {
			t.Fatalf("the refusal echoed the value: %s", w.Body.String())
		}
	}
	if s.number(`SELECT sequence FROM source_record_watermarks WHERE record_epoch=$1`, crm.epoch) != 2 ||
		s.number(`SELECT count(*) FROM source_events WHERE event_id LIKE 'crm-bad-%'`) != 0 || s.outboxFor(crm.profile) != outbox {
		t.Fatal("an invalid event left a receipt, an outbox record or a moved watermark")
	}
	// The refused sequence is still free, so the corrected event applies.
	s.receipt(s.sendEvent(tokenB, eventBody("crm-3", crm, 1, 3, `"set":{"lifetime_value":130}`)), 201, "applied")

	// Typing changes no write scope: another source cannot write this record, whatever the value.
	before := s.number(`SELECT count(*) FROM provenance WHERE profile_id=$1`, crm.profile)
	for _, body := range []string{`{"set":{"tier":"gold"}}`, `{"set":{"tier":"platinum"}}`} {
		patch := httptest.NewRequest("PATCH", "/v1/profiles/"+crm.profile, strings.NewReader(body))
		patch.Header.Set("Content-Type", "application/json")
		patch.Header.Set("Authorization", "Bearer "+tokenA)
		patch.Header.Set("Idempotency-Key", platform.UUID())
		patch.Header.Set("If-Match", fmt.Sprintf(`"%d"`, s.version(crm.profile)))
		refused := httptest.NewRecorder()
		s.h.ServeHTTP(refused, patch)
		s.expect(refused, 403)
	}
	if s.number(`SELECT count(*) FROM provenance WHERE profile_id=$1`, crm.profile) != before {
		t.Fatal("a refused write was recorded on the record")
	}

	// Undefined attributes remain untyped scalars with no recorded revision.
	untyped := s.receipt(s.sendEvent(tokenB, eventBody("crm-4", crm, 1, 4, `"set":{"nickname":"Synthetic"}`)), 201, "applied")
	if s.revisionOf(untyped.MutationID, "nickname") != 0 {
		t.Fatal("an untyped value recorded a revision")
	}

	profile := s.call("GET", "/v1/operator/profiles/"+crm.profile, "", operatorToken)
	s.expect(profile, 200)
	if !strings.Contains(profile.Body.String(), `"attribute_revision":1`) {
		t.Fatalf("profile provenance %s", profile.Body.String())
	}
}

func TestProfileWritesAreTyped(t *testing.T) {
	s := consentSetup(t)
	s.expect(s.define("orders", `{"type":"integer"}`, ""), 201)
	before := s.count("mutations")
	key := platform.UUID()
	s.expect(s.callHeader("POST", "/v1/profiles", `{"set":{"orders":1.5}}`, tokenB, key), 422)
	if s.count("mutations") != before {
		t.Fatal("a refused write reserved its key")
	}
	s.expect(s.callHeader("POST", "/v1/profiles", `{"set":{"orders":3}}`, tokenB, key), 201)
}

func TestDefinitionAdoptsOnlyConformingValues(t *testing.T) {
	s := consentSetup(t)
	good := s.sourceRecord(tokenB)
	bad := s.sourceRecord(tokenB)
	s.receipt(s.sendEvent(tokenB, eventBody("good-1", good, 1, 1, `"set":{"tier":"gold"}`)), 201, "applied")
	s.receipt(s.sendEvent(tokenB, eventBody("bad-1", bad, 1, 1, `"set":{"tier":"Gold Plus"}`)), 201, "applied")

	refused := s.define("tier", `{"type":"enum","values":["gold","silver"]}`, "")
	s.expect(refused, 409)
	if !strings.Contains(refused.Body.String(), "1 stored values") || strings.Contains(refused.Body.String(), "Gold Plus") {
		t.Fatalf("refusal %s", refused.Body.String())
	}
	// The owner removes the value; its tombstone keeps ownership and no longer blocks the definition.
	s.receipt(s.sendEvent(tokenB, eventBody("bad-2", bad, 1, 2, `"remove":["tier"]`)), 201, "applied")
	s.expect(s.define("tier", `{"type":"enum","values":["gold","silver"]}`, ""), 201)
	s.expect(s.sendEvent(tokenB, eventBody("bad-3", bad, 1, 3, `"set":{"tier":"Gold Plus"}`)), 422)
}

// A definition races untyped writes of values it would refuse. Whichever commits first, no stored
// value may violate a definition that exists.
func TestDefinitionAndConcurrentWritesStayConsistent(t *testing.T) {
	s := consentSetup(t)
	records := make([]sourceRecord, 6)
	for i := range records {
		records[i] = s.sourceRecord(tokenB)
	}
	for round := range 5 {
		path := fmt.Sprintf("score_%d", round)
		var wg sync.WaitGroup
		defined := 0
		for i, r := range records {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := fmt.Sprintf("race-%d-%d", round, i)
				w := s.sendEvent(tokenB, eventBody(id, r, 1, int64(round+1), `"set":{"`+path+`":"many"}`))
				if w.Code != 201 && w.Code != 422 {
					t.Errorf("event %d: %s", w.Code, w.Body.String())
				}
			}()
		}
		w := s.define(path, `{"type":"integer"}`, "")
		wg.Wait()
		switch w.Code {
		case 201:
			defined = 1
		case 409:
		default:
			t.Fatalf("definition %d: %s", w.Code, w.Body.String())
		}
		stored := s.number(`SELECT count(*) FROM profiles WHERE attributes ? $1`, path)
		if defined == 1 && stored != 0 {
			t.Fatalf("round %d: %d stored values violate the definition", round, stored)
		}
		if defined == 0 && stored == 0 {
			t.Fatalf("round %d: the definition was refused with nothing to refuse", round)
		}
	}
}
