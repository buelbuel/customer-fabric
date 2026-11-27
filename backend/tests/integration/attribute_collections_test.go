package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func TestCollectionDefinitionsAndSourceWrites(t *testing.T) {
	s := consentSetup(t)
	s.expect(s.callHeader("POST", "/v1/profiles", `{"set":{"interests":["travel"]}}`, tokenB, platform.UUID()), 422)
	s.expect(s.define("interests", `{"type":"enum","values":["travel","音楽","café"],"max_items":2}`, ""), 201)
	s.expect(s.define("interests", `{"type":"enum","values":["travel","音楽","café"],"max_items":1}`, `"1"`), 409)
	s.expect(s.define("interests", `{"type":"enum","values":["travel","音楽","café"]}`, `"1"`), 409)
	s.expect(s.define("interests", `{"type":"enum","values":["travel","音楽","café","books"],"max_items":3}`, `"1"`), 201)
	r := s.sourceRecord(tokenB)
	applied := s.receipt(s.sendEvent(tokenB, eventBody("collections-1", r, 1, 1, `"set":{"interests":["音楽","café"]}`)), 201, "applied")
	if s.revisionOf(applied.MutationID, "interests") != 2 {
		t.Fatal("collection provenance did not record revision")
	}
	for _, invalid := range []string{
		`["travel",null]`, `["travel","books","音楽","café"]`, `["private-value"]`,
		`"travel"`, `[["travel"]]`, `["\ud800"]`, `["\u0000"]`,
	} {
		res := s.sendEvent(tokenB, eventBody("bad-"+platform.UUID(), r, 1, 2, `"set":{"interests":`+invalid+`}`))
		s.expect(res, 422)
		if strings.Contains(res.Body.String(), "private-value") {
			t.Fatal("error disclosed a value")
		}
	}
	s.receipt(s.sendEvent(tokenB, eventBody("collections-2", r, 1, 2, `"set":{"interests":[]}`)), 201, "applied")
	s.expect(s.callHeader("POST", "/v1/profiles", `{"set":{"interests":["travel","books"]}}`, tokenA, platform.UUID()), 201)
	patch := httptest.NewRequest("PATCH", "/v1/profiles/"+r.profile, strings.NewReader(`{"set":{"interests":["travel"]}}`))
	patch.Header.Set("Content-Type", "application/json")
	patch.Header.Set("Authorization", "Bearer "+tokenA)
	patch.Header.Set("Idempotency-Key", platform.UUID())
	patch.Header.Set("If-Match", fmt.Sprintf(`"%d"`, s.version(r.profile)))
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, patch)
	s.expect(w, 403)
}

func TestCollectionRevisionRaces(t *testing.T) {
	s := consentSetup(t)
	s.expect(s.define("topics", `{"type":"string","max_items":1}`, ""), 201)
	records := make([]sourceRecord, 6)
	for i := range records {
		records[i] = s.sourceRecord(tokenB)
	}
	var wg sync.WaitGroup
	for i, r := range records {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := s.sendEvent(tokenB, eventBody(fmt.Sprintf("list-race-%d", i), r, 1, 1, `"set":{"topics":["a","b"]}`))
			if w.Code != 201 && w.Code != 422 {
				t.Errorf("event %d: %s", w.Code, w.Body.String())
			}
		}()
	}
	s.expect(s.define("topics", `{"type":"string","max_items":2}`, `"1"`), 201)
	wg.Wait()
	if n := s.number(`SELECT count(*) FROM profiles WHERE jsonb_array_length(attributes->'topics') > 2`); n != 0 {
		t.Fatalf("%d values exceed the current bound", n)
	}
	if n := s.number(`SELECT count(*) FROM provenance p JOIN profiles r ON r.id=p.profile_id
		WHERE p.path='topics' AND jsonb_array_length(r.attributes->'topics')=2 AND p.attribute_revision<>2`); n != 0 {
		t.Fatalf("%d accepted values lack the widening revision", n)
	}
}

func TestImportTypedCollections(t *testing.T) {
	s := setupLinking(t)
	s.expect(s.request("POST", "/v1/operator/attributes/interests", `{"type":"string","max_length":12,"max_items":2}`, "", viewOperator, ""), 201)
	imp := s.startImport(tokenA, `{"max_errors":10}`)
	p := progress(t, s.batch(tokenA, imp.ID, 1,
		row("sf-003", `{"interests":["voyage","音楽"]}`),
		row("sf-004", `{"interests":["voyage",null]}`),
		row("sf-005", `{"interests":["a","b","c"]}`),
		row("sf-006", `{"interests":[]}`),
	))
	if p.Created != 2 || p.Quarantined != 2 || p.Checkpoint != 4 {
		t.Fatalf("import progress %+v", p)
	}
	var value string
	if err := s.pool.QueryRow(context.Background(), `SELECT (p.attributes->'interests')::text FROM profiles p JOIN external_identifiers e ON e.profile_id=p.id WHERE e.source='console' AND e.value='sf-003'`).Scan(&value); err != nil || !strings.Contains(value, "音楽") {
		t.Fatalf("imported collection %q: %v", value, err)
	}
}

func TestGovernedViewCollectionConflictAndWithdrawal(t *testing.T) {
	s := setupLinking(t)
	s.govern("interests", `{"type":"string","max_items":3}`, `{"readers":["operator"],"sources":["console","importer"]}`)
	s.viewRecord(tokenA, "sf-collection", `{"interests":["café","音楽"]}`)
	s.viewRecord(tokenB, "shop-collection", `{"interests":["ski"]}`)
	s.viewLink(linkIssuerA, linkCustomerA, "sf-collection")
	s.viewLink(linkIssuerB, linkCustomerA, "shop-collection")
	view := s.view(linkCustomerA)
	if len(view.Attributes) != 1 || !view.Attributes[0].Conflict || len(view.Attributes[0].Values) != 2 {
		t.Fatalf("collection conflicts %+v", view.Attributes)
	}

	for _, v := range view.Attributes[0].Values {
		if v.AttributeRevision == nil || *v.AttributeRevision != 1 {
			t.Fatalf("missing revision %+v", v)
		}
	}
	s.expect(s.request("POST", "/v1/operator/attributes/interests/access", `{"readers":["operator"],"sources":["console"]}`, "", viewOperator, `"1"`), 201)
	view = s.view(linkCustomerA)
	if len(view.Attributes) != 1 || len(view.Attributes[0].Values) != 1 || view.Attributes[0].Conflict || view.Attributes[0].Values[0].Source != "console" {
		t.Fatalf("policy did not withdraw a collection %+v", view.Attributes)
	}
}

func TestThreeSourceCollectionResolution(t *testing.T) {
	s := setup(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO sources(name) VALUES('erp')`); err != nil {
		t.Fatal(err)
	}
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", OperatorToken: viewOperator,
		Tokens: map[string]string{"console": tokenA, "importer": tokenB, "erp": tokenC}})
	if err != nil {
		t.Fatal(err)
	}
	s.h = h
	s.expect(s.operatorCall("POST", "/v1/operator/attributes/interests", `{"type":"string","max_items":3}`), 201)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer", "erp")
	sf := s.record(tokenA, `{"set":{"interests":["café","音楽"]},"identifiers":[{"namespace":"salesforce","value":"sf-001"},{"namespace":"customer_number","value":"C-42"}]}`)
	shop := s.record(tokenB, `{"set":{"interests":["ski"]},"identifiers":[{"namespace":"shopify","value":"shop-001"},{"namespace":"customer_number","value":"C-42"}]}`)
	erp := s.record(tokenC, `{"set":{"interests":["books","ski"]},"identifiers":[{"namespace":"erp","value":"erp-001"},{"namespace":"customer_number","value":"C-42"}]}`)
	s.reconcile()
	c := s.customer(erp)
	var interests []string
	if err := json.Unmarshal(c.Attributes["interests"], &interests); err != nil {
		t.Fatal(err)
	}
	if len(c.Records) != 3 || c.ResolvedFrom["interests"] != sf || !slices.Equal(interests, []string{"café", "音楽"}) {
		t.Fatalf("unexpected resolution: %+v", c)
	}
	s.expect(s.operatorCall("PUT", "/v1/operator/attribute-resolution-policies/interests", `{"strategy":"source_priority","source_priority":["erp","importer","console"]}`), 200)
	c = s.customer(shop)
	if c.ResolvedFrom["interests"] != erp {
		t.Fatalf("priority did not select ERP: %+v", c.ResolvedFrom)
	}
	s.expect(s.patch(tokenC, erp, `{"remove":["interests"]}`, 1), 200)
	if c = s.customer(sf); c.ResolvedFrom["interests"] != shop {
		t.Fatalf("withdrawal did not select Shopify: %+v", c.ResolvedFrom)
	}
}
