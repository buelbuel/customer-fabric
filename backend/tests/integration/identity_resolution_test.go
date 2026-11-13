package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

// trustKey declares an identifier namespace a trusted identity key across sources, as a governance admin.
func trustKey(t *testing.T, h http.Handler, operator, namespace string, sources ...string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"sources": sources})
	r := httptest.NewRequest("PUT", "/v1/operator/identity-resolution-keys/"+namespace, strings.NewReader(string(body)))
	r.Header.Set("Authorization", "Bearer "+operator)
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 200 {
		t.Fatalf("trust key %s: %d %s", namespace, w.Code, w.Body.String())
	}
}

// record creates a source's record and returns its ID.
func (s *suite) record(token, body string) string {
	s.t.Helper()
	w := s.request("POST", "/v1/profiles", body, platform.UUID(), token, "")
	s.expect(w, 201)
	var receipt profiles.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		s.t.Fatal(err)
	}
	return receipt.ID
}

// reconcile runs the reconciler until the queue is empty.
func (s *suite) reconcile() {
	s.t.Helper()
	if _, err := (platform.Store{Pool: s.pool}).ReconcileIdentities(context.Background()); err != nil {
		s.t.Fatal(err)
	}
}

// customer reads the unified customer an ID names, as an operator.
func (s *suite) customer(id string) profiles.Customer {
	s.t.Helper()
	w := s.request("GET", "/v1/operator/profiles/"+id, "", "", viewOperator, "")
	s.expect(w, 200)
	var c profiles.Customer
	if err := json.Unmarshal(w.Body.Bytes(), &c); err != nil {
		s.t.Fatal(err)
	}
	return c
}

// recordVersion is a source record's own version, the one its writes expect.
func (s *suite) recordVersion(id string) int64 {
	s.t.Helper()
	for _, r := range s.customer(id).Records {
		if r.ID == id {
			return r.Version
		}
	}
	s.t.Fatalf("record %s missing from its customer", id)
	return 0
}

func (s *suite) patch(token, id, body string, version int64) *httptest.ResponseRecorder {
	return s.request("PATCH", "/v1/profiles/"+id, body, platform.UUID(), token, fmt.Sprintf(`"%d"`, version))
}

func (s *suite) operatorCall(method, path, body string) *httptest.ResponseRecorder {
	return s.request(method, path, body, "", viewOperator, "")
}

type identityHistory struct {
	Links     []platform.ProfileLink      `json:"links"`
	Decisions []platform.IdentityDecision `json:"decisions"`
}

func (s *suite) identityHistory(id string) identityHistory {
	s.t.Helper()
	w := s.operatorCall("GET", "/v1/operator/profiles/"+id+"/identity", "")
	s.expect(w, 200)
	var h identityHistory
	if err := json.Unmarshal(w.Body.Bytes(), &h); err != nil {
		s.t.Fatal(err)
	}
	return h
}

func (s *suite) countWhere(query string, args ...any) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

// Salesforce (console) and Shopify (importer) each keep their own record of one customer. A trusted key
// unites them: the oldest record names the customer and the other ID resolves to it. Overlapping values
// both stay; the unified view resolves them, by policy or by the earliest value still held. Each source
// writes only its own record, also through the unified ID.
func TestSourceRecordsUniteIntoOneCustomer(t *testing.T) {
	s := setup(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada := s.record(tokenA, `{"set":{"display_name":"Alice","tier":"gold"},"identifiers":[{"namespace":"crm","value":"sf-003"},{"namespace":"customer_number","value":"C-1001"}]}`)

	// A profile ID alone gives another source nothing, and looks like an unknown profile.
	refused := s.patch(tokenB, ada, `{"set":{"loyalty_points":"1"}}`, 1)
	unknown := s.patch(tokenB, platform.UUID(), `{"set":{"loyalty_points":"1"}}`, 1)
	s.expect(refused, 403)
	s.expect(unknown, 403)
	if refused.Body.String() != unknown.Body.String() {
		t.Fatalf("a refusal disclosed whether the profile exists: %s / %s", refused.Body.String(), unknown.Body.String())
	}

	// Shopify's record overlaps on display_name; it is accepted whole and keeps its own value.
	shop := s.record(tokenB, `{"set":{"display_name":"Alicia","loyalty_points":"120"},"identifiers":[{"namespace":"shop","value":"shop-77"},{"namespace":"customer_number","value":"C-1001"}]}`)
	if shop == ada {
		t.Fatal("a source wrote another source's record")
	}
	s.reconcile()
	c := s.customer(shop)
	if c.ID != ada || !slices.Equal(c.Aliases, []string{shop}) || len(c.Records) != 2 {
		t.Fatalf("customer %s aliases %v records %d", c.ID, c.Aliases, len(c.Records))
	}
	if string(c.Attributes["display_name"]) != `"Alice"` || c.ResolvedFrom["display_name"] != ada ||
		string(c.Attributes["loyalty_points"]) != `"120"` || string(c.Attributes["tier"]) != `"gold"` {
		t.Fatalf("resolved %v from %v", c.Attributes, c.ResolvedFrom)
	}
	if string(c.Records[1].Attributes["display_name"]) != `"Alicia"` || c.Records[1].Source != "importer" {
		t.Fatalf("the alternative value was not kept: %+v", c.Records[1])
	}
	if len(c.Identifiers) != 4 {
		t.Fatalf("identifiers %+v", c.Identifiers)
	}

	// Shopify writes the unified ID: the write reaches its own record, under its own version.
	w := s.patch(tokenB, ada, `{"set":{"tier":"platinum"}}`, 1)
	s.expect(w, 200)
	var receipt profiles.Receipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	if receipt.ID != shop || receipt.Version != 2 {
		t.Fatalf("routed receipt %+v", receipt)
	}
	s.expect(s.patch(tokenB, ada, `{"set":{"tier":"silver"}}`, 1), 412)
	if c = s.customer(ada); string(c.Attributes["tier"]) != `"gold"` {
		t.Fatalf("a later overlapping value displaced the held one: %s", c.Attributes["tier"])
	}
	// A policy decides instead; removing it returns to the held value.
	s.expect(s.operatorCall("PUT", "/v1/operator/attribute-resolution-policies/tier", `{"strategy":"source_priority","source_priority":["importer","console"]}`), 200)
	if c = s.customer(ada); string(c.Attributes["tier"]) != `"platinum"` || c.ResolvedFrom["tier"] != shop {
		t.Fatalf("priority policy gave %s", c.Attributes["tier"])
	}
	s.expect(s.operatorCall("DELETE", "/v1/operator/attribute-resolution-policies/tier", ""), 204)
	// Salesforce withdraws its value: only its contribution goes, and Shopify's value shows.
	s.expect(s.patch(tokenA, ada, `{"remove":["display_name"]}`, 1), 200)
	if c = s.customer(ada); string(c.Attributes["display_name"]) != `"Alicia"` || string(c.Attributes["tier"]) != `"gold"` {
		t.Fatalf("after withdrawal %v", c.Attributes)
	}
	// Shopify's events for its record keep reaching that record.
	event := `{"event_id":"shop-e1","type":"record.updated.v1","record":{"namespace":"shop","value":"shop-77"},"source_epoch":1,"sequence":1,"occurred_at":"2026-01-01T00:00:00Z","set":{"loyalty_points":"130"}}`
	applied := s.request("POST", "/v1/source-events", event, "", tokenB, "")
	if applied.Code != 201 || !strings.Contains(applied.Body.String(), `"outcome":"applied"`) || !strings.Contains(applied.Body.String(), shop) {
		t.Fatalf("event %d %s", applied.Code, applied.Body.String())
	}

	// The alias got one profile.unified.v1 naming the customer, carrying its version at the time.
	var canonical string
	var version int64
	if err := s.pool.QueryRow(context.Background(), `SELECT payload->>'canonical_profile_id', version FROM outbox WHERE type = 'profile.unified.v1' AND profile_id = $1`, shop).Scan(&canonical, &version); err != nil || canonical != ada || version != 1 {
		t.Fatalf("unified event %q %d %v", canonical, version, err)
	}
	if s.countWhere(`SELECT count(*) FROM outbox WHERE type = 'profile.unified.v1'`) != 1 {
		t.Fatal("the canonical record was announced as changed")
	}
	h := s.identityHistory(shop)
	if len(h.Links) != 1 || h.Links[0].KeyNamespace != "customer_number" || h.Links[0].RemovedAt != nil {
		t.Fatalf("links %+v", h.Links)
	}
	actions := map[string]int{}
	for _, d := range h.Decisions {
		actions[d.Action]++
	}
	if actions["linked"] != 2 || actions["canonical_changed"] != 1 {
		t.Fatalf("decisions %+v", h.Decisions)
	}
}

// Keys may be declared after records arrive: existing records are resolved by them, and a key added to a
// record later unites it too. Removing the key separates what it united.
func TestKeysUniteExistingRecordsRetroactively(t *testing.T) {
	s := setup(t)
	ada := s.record(tokenA, `{"set":{"display_name":"Ada"},"identifiers":[{"namespace":"customer_number","value":"C-7"},{"namespace":"loyalty_id","value":"L-7"}]}`)
	shop := s.record(tokenB, `{"set":{"display_name":"Ada L"},"identifiers":[{"namespace":"customer_number","value":"C-7"}]}`)
	late := s.record(tokenB, `{"set":{"points":"3"}}`)
	s.reconcile()
	if s.customer(shop).ID != shop {
		t.Fatal("records were united without a trusted key")
	}
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	s.reconcile()
	if c := s.customer(shop); c.ID != ada || len(c.Records) != 2 {
		t.Fatalf("the key did not unite existing records: %s %d", c.ID, len(c.Records))
	}
	trustKey(t, s.h, viewOperator, "loyalty_id", "console", "importer")
	s.expect(s.patch(tokenB, late, `{"identifiers":[{"namespace":"loyalty_id","value":"L-7"}]}`, 1), 200)
	s.reconcile()
	if c := s.customer(late); c.ID != ada || len(c.Records) != 3 {
		t.Fatalf("a key added later did not unite: %s %d", c.ID, len(c.Records))
	}
	// Writing the unified ID is now ambiguous for Shopify, which has two records in it.
	s.expect(s.patch(tokenB, ada, `{"set":{"points":"4"}}`, 2), 409)

	s.expect(s.operatorCall("DELETE", "/v1/operator/identity-resolution-keys/customer_number", ""), 204)
	s.reconcile()
	if c := s.customer(shop); c.ID != shop || len(c.Records) != 1 {
		t.Fatalf("still unified after the key went: %s", c.ID)
	}
	if c := s.customer(late); c.ID != ada || len(c.Records) != 2 {
		t.Fatalf("the other key's link went too: %s", c.ID)
	}
	if s.countWhere(`SELECT count(*) FROM outbox WHERE type = 'profile.unified.v1' AND payload->>'canonical_profile_id' = profile_id::text`) != 1 {
		t.Fatal("the separated record was not announced")
	}
	if s.countWhere(`SELECT count(*) FROM profile_links WHERE removal_reason = 'key_changed'`) == 0 {
		t.Fatal("removed links lost their history")
	}
}

// A record whose key values contradict a customer's is never united with it, nor is anything by an
// untrusted namespace or source.
func TestContradictoryAndUntrustedMatchesNeverUnite(t *testing.T) {
	s := setup(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO sources(name) VALUES('loyalty')`); err != nil {
		t.Fatal(err)
	}
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", OperatorToken: viewOperator,
		Tokens: map[string]string{"console": tokenA, "importer": tokenB, "loyalty": tokenC}})
	if err != nil {
		t.Fatal(err)
	}
	s.h = h
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada := s.record(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"C-1"},{"namespace":"email_hash","value":"h1"}]}`)
	bob := s.record(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"C-2"}]}`)
	both := s.record(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"C-1"},{"namespace":"customer_number","value":"C-2"}]}`)
	email := s.record(tokenB, `{"identifiers":[{"namespace":"email_hash","value":"h1"}]}`)
	loyal := s.record(tokenC, `{"identifiers":[{"namespace":"customer_number","value":"C-1"}]}`)
	s.reconcile()
	for _, id := range []string{ada, bob, both, email, loyal} {
		if c := s.customer(id); len(c.Records) != 1 {
			t.Fatalf("%s was united: %+v", id, c.Records)
		}
	}
	conflicts := s.countWhere(`SELECT count(*) FROM identity_decisions WHERE action = 'conflict' AND profile_id = $1`, both)
	if conflicts == 0 {
		t.Fatal("the contradiction was not recorded")
	}
	// Reconciling the same state again decides nothing new.
	decisions := s.count("identity_decisions")
	s.expect(s.operatorCall("PUT", "/v1/operator/identity-resolution-keys/customer_number", `{"sources":["console","importer"]}`), 200)
	s.reconcile()
	if s.count("identity_decisions") != decisions {
		t.Fatal("a repeated reconciliation recorded new decisions")
	}
	// Once its source is trusted, the loyalty record joins the one customer it agrees with.
	s.expect(s.operatorCall("PUT", "/v1/operator/identity-resolution-keys/customer_number", `{"sources":["console","importer","loyalty"]}`), 200)
	s.reconcile()
	if c := s.customer(loyal); c.ID != ada || len(c.Records) != 2 {
		t.Fatalf("a consistent record did not unite: %s %d", c.ID, len(c.Records))
	}
	if c := s.customer(both); len(c.Records) != 1 {
		t.Fatal("the contradictory record was united")
	}
}

// An identity reviewer corrects a wrong link: the records separate at once and the reconciler never
// links them by that key again. The history keeps the link and who removed it.
func TestCorrectedLinksStaySeparated(t *testing.T) {
	s := setup(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada := s.record(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"C-9"}]}`)
	shop := s.record(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"C-9"}]}`)
	s.reconcile()
	link := s.identityHistory(ada).Links[0]
	path := "/v1/operator/profile-links/" + link.ID + "/remove"
	s.expect(s.operatorCall("POST", path, `{"reference":"bad ref"}`), 422)
	s.expect(s.operatorCall("POST", "/v1/operator/profile-links/"+platform.UUID()+"/remove", `{"reference":"TICKET-1"}`), 404)
	removed := s.operatorCall("POST", path, `{"reference":"TICKET-1"}`)
	s.expect(removed, 200)
	if !strings.Contains(removed.Body.String(), `"removal_reason":"corrected"`) {
		t.Fatalf("removal %s", removed.Body.String())
	}
	s.expect(s.operatorCall("POST", path, `{"reference":"TICKET-1"}`), 409)
	if c := s.customer(shop); c.ID != shop {
		t.Fatal("the correction did not separate the records")
	}
	s.expect(s.patch(tokenB, shop, `{"set":{"points":"1"}}`, 1), 200)
	s.reconcile()
	if c := s.customer(shop); c.ID != shop {
		t.Fatal("the reconciler relinked a corrected pair")
	}
	h := s.identityHistory(ada)
	if len(h.Links) != 1 || h.Links[0].RemovalReference != "TICKET-1" || h.Links[0].RemovedBy != "development-bearer" {
		t.Fatalf("history %+v", h.Links)
	}
	// The correction concerns that key only: another trusted key naming both unites them again.
	trustKey(t, s.h, viewOperator, "loyalty_id", "console", "importer")
	s.expect(s.patch(tokenA, ada, `{"identifiers":[{"namespace":"loyalty_id","value":"L-9"}]}`, 1), 200)
	s.expect(s.patch(tokenB, shop, `{"identifiers":[{"namespace":"loyalty_id","value":"L-9"}]}`, 2), 200)
	s.reconcile()
	if c := s.customer(shop); c.ID != ada || len(s.identityHistory(ada).Links) != 2 {
		t.Fatalf("after correction: %s", c.ID)
	}
}

// A link held back because it contradicted a customer is made once the contradicting link goes.
func TestLinksHeldBackByAConflictFollowItsRemoval(t *testing.T) {
	s := setup(t)
	for _, key := range []string{"email_hash", "customer_number", "loyalty_id"} {
		trustKey(t, s.h, viewOperator, key, "console", "importer")
	}
	ada := s.record(tokenA, `{"identifiers":[{"namespace":"email_hash","value":"e1"},{"namespace":"loyalty_id","value":"L1"}]}`)
	shop := s.record(tokenB, `{"identifiers":[{"namespace":"email_hash","value":"e1"},{"namespace":"customer_number","value":"K"}]}`)
	other := s.record(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"K"},{"namespace":"loyalty_id","value":"L2"}]}`)
	s.reconcile()
	// Shopify's record matches both, but the two cannot share a customer: one link stands.
	links := slices.DeleteFunc(s.identityHistory(shop).Links, func(l platform.ProfileLink) bool { return l.RemovedAt != nil })
	if len(links) != 1 || len(s.customer(shop).Records) != 2 {
		t.Fatalf("before correction: %+v", links)
	}
	held := ada
	if links[0].Profiles[0] == ada || links[0].Profiles[1] == ada {
		held = other
	}
	s.expect(s.operatorCall("POST", "/v1/operator/profile-links/"+links[0].ID+"/remove", `{"reference":"TICKET-2"}`), 200)
	s.reconcile()
	if c := s.customer(held); len(c.Records) != 2 || !slices.ContainsFunc(c.Records, func(r profiles.SourceRecord) bool { return r.ID == shop }) {
		t.Fatalf("the held-back link was not made: %s with %d records", c.ID, len(c.Records))
	}
}

// A restricted record leaves its customer and returns when the restriction is lifted.
func TestRestrictedRecordsLeaveTheirCustomer(t *testing.T) {
	s := setup(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada := s.record(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"C-3"}]}`)
	shop := s.record(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"C-3"}]}`)
	s.reconcile()
	reviewBy := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	restriction := "/v1/operator/profiles/" + ada + "/restriction"
	s.expect(s.operatorCall("POST", restriction, fmt.Sprintf(`{"action":"restrict","reason":"legal_claims","review_by":%q}`, reviewBy)), 200)
	s.reconcile()
	if c := s.customer(shop); c.ID != shop || len(c.Records) != 1 {
		t.Fatalf("a restricted record stayed united: %s", c.ID)
	}
	s.expect(s.operatorCall("POST", restriction, `{"action":"lift","reason":"claim_resolved"}`), 200)
	s.reconcile()
	if c := s.customer(shop); c.ID != ada {
		t.Fatalf("the lifted record did not return: %s", c.ID)
	}
}

// An import row creating a record presenting a trusted key unites like a direct create.
func TestImportedRecordsUnite(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada := s.record(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"K5"}]}`)
	imp := s.startImport(tokenB, `{"max_errors":0}`)
	done := progress(t, s.batch(tokenB, imp.ID, 1, `{"record":{"namespace":"customer_number","value":"K5"},"set":{"loyalty_points":"7"}}`))
	if done.Created != 1 {
		t.Fatalf("progress %+v", done)
	}
	s.reconcile()
	if c := s.suite.customer(ada); len(c.Records) != 2 || string(c.Attributes["loyalty_points"]) != `"7"` {
		t.Fatalf("imported record %+v", c)
	}
}

// Records and reconcilers running at once still give every key one customer, and a second pass changes
// nothing.
func TestConcurrentRecordsAndReconcilersConverge(t *testing.T) {
	s := setup(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	store := platform.Store{Pool: s.pool}
	ids := make([][2]string, 6)
	var wg sync.WaitGroup
	for round := range ids {
		for i, token := range []string{tokenA, tokenB} {
			wg.Go(func() {
				w := s.request("POST", "/v1/profiles", fmt.Sprintf(`{"identifiers":[{"namespace":"customer_number","value":"C-%d"}]}`, round), platform.UUID(), token, "")
				var receipt profiles.Receipt
				if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || w.Code != 201 {
					t.Errorf("create %d", w.Code)
				}
				ids[round][i] = receipt.ID
			})
		}
		wg.Go(func() {
			if _, err := store.ReconcileIdentities(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	s.reconcile()
	for _, pair := range ids {
		if a, b := s.customer(pair[0]), s.customer(pair[1]); a.ID != b.ID || len(a.Records) != 2 {
			t.Fatalf("pair %v did not converge: %s / %s", pair, a.ID, b.ID)
		}
	}
	decisions, events := s.count("identity_decisions"), s.countWhere(`SELECT count(*) FROM outbox WHERE type = 'profile.unified.v1'`)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO identity_reconciliation_queue (profile_id) SELECT id FROM profiles`); err != nil {
		t.Fatal(err)
	}
	s.reconcile()
	if s.count("identity_decisions") != decisions || s.countWhere(`SELECT count(*) FROM outbox WHERE type = 'profile.unified.v1'`) != events {
		t.Fatal("reconciling unchanged state decided again")
	}
}

// A customer's profile anchor carries no source identifiers, so no source writes it or unites with it.
func TestCustomerAnchorsStayOutOfReach(t *testing.T) {
	s := setupLinking(t)
	anchor := s.anchor(linkCustomerA)
	s.expect(s.request("PATCH", "/v1/profiles/"+anchor, `{"set":{"tier":"gold"}}`, platform.UUID(), tokenB, `"1"`), 403)
	s.expect(s.request("PATCH", "/v1/profiles/"+anchor, `{"identifiers":[{"namespace":"crm","value":"x"}]}`, platform.UUID(), tokenA, `"1"`), 403)
}

// Keys and policies need the governance_admin role and corrections identity_reviewer; reading them
// needs profile_reader.
func TestIdentityAdministrationNeedsItsRoles(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "profile_reader")
	h := s.kratosAPI(issuer.url)
	keys, policies := "/v1/operator/identity-resolution-keys", "/v1/operator/attribute-resolution-policies"
	if w := operatorSession(h, "PUT", keys+"/customer_number", `{"sources":["console","importer"]}`); w.Code != 403 {
		t.Fatalf("a profile reader declared a key: %d", w.Code)
	}
	if w := operatorSession(h, "PUT", policies+"/tier", `{"strategy":"most_recent"}`); w.Code != 403 {
		t.Fatalf("a profile reader set a policy: %d", w.Code)
	}
	if w := operatorSession(h, "POST", "/v1/operator/profile-links/"+platform.UUID()+"/remove", `{"reference":"T-1"}`); w.Code != 403 {
		t.Fatalf("a profile reader corrected a link: %d", w.Code)
	}
	for _, path := range []string{keys, policies} {
		if w := operatorSession(h, "GET", path, ""); w.Code != 200 {
			t.Fatalf("a profile reader could not list %s: %d", path, w.Code)
		}
	}
	issuer.assign("profile_reader", "governance_admin")
	for body, status := range map[string]int{`{"sources":["console"]}`: 422, `{"sources":["console","nosuch"]}`: 422, `{"sources":["console","importer"]}`: 200} {
		if w := operatorSession(h, "PUT", keys+"/customer_number", body); w.Code != status {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for body, status := range map[string]int{`{"strategy":"oldest"}`: 422, `{"strategy":"most_recent","source_priority":["console"]}`: 422,
		`{"strategy":"source_priority"}`: 422, `{"strategy":"source_priority","source_priority":["console","console"]}`: 422,
		`{"strategy":"source_priority","source_priority":["nosuch"]}`: 422, `{"strategy":"source_priority","source_priority":["importer","console"]}`: 200} {
		if w := operatorSession(h, "PUT", policies+"/tier", body); w.Code != status {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	for _, path := range []string{keys + "/customer_number", policies + "/tier"} {
		if w := operatorSession(h, "DELETE", path, ""); w.Code != 204 {
			t.Fatalf("delete %s %d", path, w.Code)
		}
		if w := operatorSession(h, "DELETE", path, ""); w.Code != 404 {
			t.Fatalf("second delete %s %d", path, w.Code)
		}
	}
}

// tokenC is a third source, loyalty, that no trusted key lists.
const tokenC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccd"
