package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// createRecord posts a source's new record and returns the profile it resolved to, with the status.
func (s *suite) resolveRecord(token, body string) (string, int) {
	s.t.Helper()
	w := s.request("POST", "/v1/profiles", body, platform.UUID(), token, "")
	var receipt profiles.Receipt
	_ = json.Unmarshal(w.Body.Bytes(), &receipt)
	return receipt.ID, w.Code
}

func (s *suite) operatorVersion(id string) int64 {
	s.t.Helper()
	w := s.request("GET", "/v1/operator/profiles/"+id, "", "", viewOperator, "")
	s.expect(w, 200)
	var p profiles.Profile
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		s.t.Fatal(err)
	}
	return p.Version
}

func (s *suite) patchCurrent(token, id, body string) int {
	s.t.Helper()
	return s.request("PATCH", "/v1/profiles/"+id, body, platform.UUID(), token, fmt.Sprintf(`"%d"`, s.operatorVersion(id))).Code
}

// Salesforce (console) creates a profile; Shopify (importer) creates its own record presenting the same
// trusted identity key and joins that profile. From then on Shopify contributes to it through the public
// API, under field ownership, tombstones and optimistic concurrency. Knowing a profile ID grants nothing.
func TestSourcesJoinOneProfileThroughTrustedIdentityKeys(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada, code := s.resolveRecord(tokenA, `{"set":{"display_name":"Synthetic Ada","tier":"gold"},"identifiers":[{"namespace":"crm","value":"sf-003"},{"namespace":"customer_number","value":"C-1001"}]}`)
	if code != 201 {
		t.Fatalf("salesforce create %d", code)
	}

	// Before Shopify is associated, a profile ID gives it nothing, and it looks like an unknown profile.
	refused := s.request("PATCH", "/v1/profiles/"+ada, `{"set":{"loyalty_points":"1"}}`, platform.UUID(), tokenB, `"1"`)
	unknown := s.request("PATCH", "/v1/profiles/"+platform.UUID(), `{"set":{"loyalty_points":"1"}}`, platform.UUID(), tokenB, `"1"`)
	s.expect(refused, 403)
	s.expect(unknown, 403)
	if refused.Body.String() != unknown.Body.String() {
		t.Fatalf("a refusal disclosed whether the profile exists: %s / %s", refused.Body.String(), unknown.Body.String())
	}
	// Naming its own identifier on the profile does not associate it either.
	s.expect(s.request("PATCH", "/v1/profiles/"+ada, `{"identifiers":[{"namespace":"shop","value":"shop-77"}]}`, platform.UUID(), tokenB, `"1"`), 403)

	// Shopify's new record presents the key and joins Salesforce's profile.
	joined, code := s.resolveRecord(tokenB, `{"set":{"loyalty_points":"120"},"identifiers":[{"namespace":"shop","value":"shop-77"},{"namespace":"customer_number","value":"C-1001"}]}`)
	if code != 201 || joined != ada {
		t.Fatalf("shopify resolved to %s (%d), want %s", joined, code, ada)
	}
	if code := s.patchCurrent(tokenB, ada, `{"set":{"display_name":"Shopify name"}}`); code != 409 {
		t.Fatalf("a contributor overwrote another source's attribute: %d", code)
	}
	if code := s.patchCurrent(tokenA, ada, `{"remove":["tier"]}`); code != 200 {
		t.Fatalf("salesforce removal %d", code)
	}
	if code := s.patchCurrent(tokenB, ada, `{"set":{"tier":"platinum"}}`); code != 409 {
		t.Fatalf("a contributor resurrected another source's tombstone: %d", code)
	}
	if code := s.patchCurrent(tokenB, ada, `{"set":{"loyalty_points":"130"},"identifiers":[{"namespace":"shop","value":"shop-77-alias"}]}`); code != 200 {
		t.Fatalf("an associated source updating its own attribute and adding its own alias: %d", code)
	}
	s.expect(s.request("PATCH", "/v1/profiles/"+ada, `{"set":{"loyalty_points":"140"}}`, platform.UUID(), tokenB, `"1"`), 412)
	// Shopify's events for its record now reach the joined profile.
	event := `{"event_id":"shop-e1","type":"record.updated.v1","record":{"namespace":"shop","value":"shop-77"},"source_epoch":1,"sequence":1,"occurred_at":"2026-01-01T00:00:00Z","set":{"loyalty_points":"130"}}`
	applied := s.request("POST", "/v1/source-events", event, "", tokenB, "")
	if applied.Code != 201 || !strings.Contains(applied.Body.String(), `"outcome":"applied"`) || !strings.Contains(applied.Body.String(), ada) {
		t.Fatalf("event %d %s", applied.Code, applied.Body.String())
	}

	w := s.request("GET", "/v1/operator/profiles/"+ada, "", "", viewOperator, "")
	s.expect(w, 200)
	var p profiles.Profile
	if err := json.Unmarshal(w.Body.Bytes(), &p); err != nil {
		t.Fatal(err)
	}
	if string(p.Attributes["display_name"]) != `"Synthetic Ada"` || string(p.Attributes["loyalty_points"]) != `"130"` || p.Attributes["tier"] != nil {
		t.Fatalf("attributes %v", p.Attributes)
	}
	sources := map[string]int{}
	for _, id := range p.Identifiers {
		sources[id.Source]++
	}
	if sources["console"] != 2 || sources["importer"] != 3 {
		t.Fatalf("identifiers %+v", p.Identifiers)
	}
	rejected := false
	for _, d := range p.Provenance {
		rejected = rejected || (d.Source == "importer" && d.Outcome == "rejected" && d.Path == "display_name")
	}
	if !rejected {
		t.Fatal("the refused overwrite left no rejected provenance")
	}
	trail := s.request("GET", "/v1/operator/profiles/"+ada+"/identity-resolutions", "", "", viewOperator, "")
	s.expect(trail, 200)
	if !strings.Contains(trail.Body.String(), `"source":"console","outcome":"created","key_namespace":"customer_number"`) ||
		!strings.Contains(trail.Body.String(), `"source":"importer","outcome":"associated","key_namespace":"customer_number"`) {
		t.Fatalf("resolution trail %s", trail.Body.String())
	}
}

// Only a trusted key presented by a trusted source resolves. Anything else, and any ambiguity, gives
// the record its own profile: nothing is ever merged.
func TestResolutionNeverJoinsWithoutAnUnambiguousTrustedMatch(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada, _ := s.resolveRecord(tokenA, `{"identifiers":[{"namespace":"crm","value":"a"},{"namespace":"customer_number","value":"C-1"},{"namespace":"email_hash","value":"h1"}]}`)
	bob, _ := s.resolveRecord(tokenA, `{"identifiers":[{"namespace":"crm","value":"b"},{"namespace":"customer_number","value":"C-2"}]}`)

	// A namespace that is not a trusted key never resolves, however the values match.
	if id, code := s.resolveRecord(tokenB, `{"identifiers":[{"namespace":"email_hash","value":"h1"}]}`); code != 201 || id == ada {
		t.Fatalf("an untrusted namespace resolved: %s %d", id, code)
	}
	// Two keys naming two profiles are ambiguous: the record gets its own profile.
	ambiguous, code := s.resolveRecord(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"C-1"},{"namespace":"customer_number","value":"C-2"}]}`)
	if code != 201 || ambiguous == ada || ambiguous == bob {
		t.Fatalf("an ambiguous record joined: %s %d", ambiguous, code)
	}
	trail := s.request("GET", "/v1/operator/profiles/"+ambiguous+"/identity-resolutions", "", "", viewOperator, "")
	if !strings.Contains(trail.Body.String(), `"outcome":"ambiguous"`) {
		t.Fatalf("ambiguity was not recorded: %s", trail.Body.String())
	}
}

// Records of different sources presenting the same key at the same time resolve one after another: one
// creates the profile and the other joins it.
func TestConcurrentRecordsWithOneKeyResolveToOneProfile(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	for round := range 8 {
		var wg sync.WaitGroup
		ids := make(chan string, 2)
		for i, token := range []string{tokenA, tokenB} {
			wg.Go(func() {
				id, code := s.resolveRecord(token, fmt.Sprintf(`{"identifiers":[{"namespace":"rec","value":"r-%d-%d"},{"namespace":"customer_number","value":"C-%d"}]}`, round, i, round))
				if code != 201 {
					t.Errorf("round %d create %d: %d", round, i, code)
				}
				ids <- id
			})
		}
		wg.Wait()
		close(ids)
		if a, b := <-ids, <-ids; a != b {
			t.Fatalf("round %d: one key resolved to two profiles", round)
		}
	}
}

// A customer's profile anchor carries no source identifiers, so no source writes it or resolves to it.
func TestCustomerAnchorsStayOutOfReach(t *testing.T) {
	s := setupLinking(t)
	anchor := s.anchor(linkCustomerA)
	s.expect(s.request("PATCH", "/v1/profiles/"+anchor, `{"set":{"tier":"gold"}}`, platform.UUID(), tokenB, `"1"`), 403)
	s.expect(s.request("PATCH", "/v1/profiles/"+anchor, `{"identifiers":[{"namespace":"crm","value":"x"}]}`, platform.UUID(), tokenA, `"1"`), 403)
}

// Declaring trusted keys needs the governance_admin role; reading them needs profile_reader.
func TestTrustedKeysNeedTheGovernanceAdminRole(t *testing.T) {
	s := consentSetup(t)
	issuer := newRolesIssuer(t, "profile_reader")
	h := s.kratosAPI(issuer.url)
	keys := "/v1/operator/identity-resolution-keys"
	if w := operatorSession(h, "PUT", keys+"/customer_number", `{"sources":["console","importer"]}`); w.Code != 403 {
		t.Fatalf("a profile reader declared a key: %d", w.Code)
	}
	if w := operatorSession(h, "GET", keys, ""); w.Code != 200 {
		t.Fatalf("a profile reader could not list keys: %d", w.Code)
	}
	issuer.assign("profile_reader", "governance_admin")
	for body, status := range map[string]int{`{"sources":["console"]}`: 422, `{"sources":["console","nosuch"]}`: 422, `{"sources":["console","importer"]}`: 200} {
		if w := operatorSession(h, "PUT", keys+"/customer_number", body); w.Code != status {
			t.Fatalf("%s: %d %s", body, w.Code, w.Body.String())
		}
	}
	if w := operatorSession(h, "DELETE", keys+"/customer_number", ""); w.Code != 204 {
		t.Fatalf("delete %d", w.Code)
	}
	if w := operatorSession(h, "DELETE", keys+"/customer_number", ""); w.Code != 404 {
		t.Fatalf("second delete %d", w.Code)
	}
}

// join associates joinerSource with profile the production way: the creating source adds a trusted
// identity key to its record, and the joining source's new record presents the same key. version is the
// profile's current version; join returns the version after both writes.
func join(t *testing.T, h http.Handler, operator, profile string, version int64, creatorToken, creatorSource, joinerToken, joinerSource string) int64 {
	t.Helper()
	trustKey(t, h, operator, "join_key", creatorSource, joinerSource)
	value := "join-" + platform.UUID()
	send := func(method, path, token, body, etag string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Idempotency-Key", platform.UUID())
		if etag != "" {
			r.Header.Set("If-Match", etag)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	if w := send("PATCH", "/v1/profiles/"+profile, creatorToken, fmt.Sprintf(`{"identifiers":[{"namespace":"join_key","value":%q}]}`, value), fmt.Sprintf(`"%d"`, version)); w.Code != 200 {
		t.Fatalf("creator key: %d %s", w.Code, w.Body.String())
	}
	w := send("POST", "/v1/profiles", joinerToken, fmt.Sprintf(`{"identifiers":[{"namespace":"join_key","value":%q}]}`, value), "")
	var receipt profiles.Receipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil || w.Code != 201 || receipt.ID != profile {
		t.Fatalf("joining record: %d %s", w.Code, w.Body.String())
	}
	return version + 2
}

// tokenC is a third source, loyalty, that no trusted key lists.
const tokenC = "cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccd"

// setupThreeSources adds the loyalty source to the linking suite.
func setupThreeSources(t *testing.T) linkingSuite {
	t.Helper()
	s := setupLinking(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO sources(name) VALUES('loyalty')`); err != nil {
		t.Fatal(err)
	}
	s.config.Tokens = map[string]string{"console": tokenA, "importer": tokenB, "loyalty": tokenC}
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.h = h
	return s
}

// Only sources a key lists resolve by it, and only those sources' identifiers count as matches.
func TestResolutionTrustsOnlyListedSources(t *testing.T) {
	s := setupThreeSources(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada, _ := s.resolveRecord(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"K1"}]}`)
	// An unlisted source presenting a trusted key resolves nothing.
	if id, code := s.resolveRecord(tokenC, `{"identifiers":[{"namespace":"customer_number","value":"K1"}]}`); code != 201 || id == ada {
		t.Fatalf("an unlisted source joined: %s %d", id, code)
	}
	// A listed source matching only an unlisted source's identifier in the namespace joins nothing.
	loyal, _ := s.resolveRecord(tokenC, `{"identifiers":[{"namespace":"customer_number","value":"K2"}]}`)
	if id, code := s.resolveRecord(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"K2"}]}`); code != 201 || id == loyal {
		t.Fatalf("an unlisted source's identifier was evidence: %s %d", id, code)
	}
}

// A restricted or erasure-pending profile is never joined: the record gets its own profile, and the
// unassociated source learns nothing about the other profile's state.
func TestResolutionSkipsRestrictedAndErasingProfiles(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	restricted, _ := s.resolveRecord(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"K3"}]}`)
	reviewBy := time.Now().AddDate(0, 1, 0).Format(time.DateOnly)
	s.expect(s.request("POST", "/v1/operator/profiles/"+restricted+"/restriction", fmt.Sprintf(`{"action":"restrict","reason":"legal_claims","review_by":%q}`, reviewBy), "", viewOperator, ""), 200)
	if id, code := s.resolveRecord(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"K3"}]}`); code != 201 || id == restricted {
		t.Fatalf("a restricted profile was joined or disclosed: %s %d", id, code)
	}
	erasing, _ := s.resolveRecord(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"K4"}]}`)
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET lifecycle_state = 'erasure_pending' WHERE id = $1`, erasing); err != nil {
		t.Fatal(err)
	}
	if id, code := s.resolveRecord(tokenB, `{"identifiers":[{"namespace":"customer_number","value":"K4"}]}`); code != 201 || id == erasing {
		t.Fatalf("an erasing profile was joined: %s %d", id, code)
	}
}

// An import row whose record presents a trusted key joins like a direct create, and counts as applied.
func TestImportRowsJoinThroughTrustedKeys(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada, _ := s.resolveRecord(tokenA, `{"identifiers":[{"namespace":"customer_number","value":"K5"}]}`)
	imp := s.startImport(tokenB, `{"max_errors":0}`)
	done := progress(t, s.batch(tokenB, imp.ID, 1, `{"record":{"namespace":"customer_number","value":"K5"},"set":{"loyalty_points":"7"}}`))
	if done.Applied != 1 || done.Created != 0 {
		t.Fatalf("progress %+v", done)
	}
	trail := s.request("GET", "/v1/operator/profiles/"+ada+"/identity-resolutions", "", "", viewOperator, "")
	if !strings.Contains(trail.Body.String(), `"source":"importer","outcome":"associated"`) {
		t.Fatalf("trail %s", trail.Body.String())
	}
}

// A join refused for an ownership conflict associates nothing; a replayed join returns its receipt; a
// source re-presenting an identifier it holds is refused, never written without its version.
func TestJoinsAreAllOrNothingAndIdempotent(t *testing.T) {
	s := setupLinking(t)
	trustKey(t, s.h, viewOperator, "customer_number", "console", "importer")
	ada, _ := s.resolveRecord(tokenA, `{"set":{"display_name":"Synthetic Ada"},"identifiers":[{"namespace":"customer_number","value":"K6"}]}`)
	if _, code := s.resolveRecord(tokenB, `{"set":{"display_name":"Overwrite"},"identifiers":[{"namespace":"shop","value":"s-6"},{"namespace":"customer_number","value":"K6"}]}`); code != 409 {
		t.Fatalf("a conflicting join %d", code)
	}
	if s.request("PATCH", "/v1/profiles/"+ada, `{"set":{"loyalty_points":"1"}}`, platform.UUID(), tokenB, fmt.Sprintf(`"%d"`, s.operatorVersion(ada))).Code != 403 {
		t.Fatal("a refused join associated the source")
	}
	trail := s.request("GET", "/v1/operator/profiles/"+ada+"/identity-resolutions", "", "", viewOperator, "")
	if strings.Contains(trail.Body.String(), `"source":"importer"`) {
		t.Fatalf("a refused join was recorded: %s", trail.Body.String())
	}

	key := platform.UUID()
	body := `{"set":{"loyalty_points":"5"},"identifiers":[{"namespace":"customer_number","value":"K6"}]}`
	first := s.request("POST", "/v1/profiles", body, key, tokenB, "")
	replay := s.request("POST", "/v1/profiles", body, key, tokenB, "")
	s.expect(first, 201)
	var firstReceipt, replayReceipt profiles.Receipt
	_ = json.Unmarshal(first.Body.Bytes(), &firstReceipt)
	_ = json.Unmarshal(replay.Body.Bytes(), &replayReceipt)
	if replay.Code != 201 || replayReceipt != firstReceipt || firstReceipt.ID != ada {
		t.Fatalf("join replay %d %s / %s", replay.Code, replay.Body.String(), first.Body.String())
	}
	var eventType string
	if err := s.pool.QueryRow(context.Background(), `SELECT type FROM outbox WHERE profile_id = $1 ORDER BY version DESC LIMIT 1`, ada).Scan(&eventType); err != nil || eventType != "profile.updated.v1" {
		t.Fatalf("join outbox %q %v", eventType, err)
	}
	version := s.operatorVersion(ada)
	if _, code := s.resolveRecord(tokenB, `{"set":{"loyalty_points":"99"},"identifiers":[{"namespace":"customer_number","value":"K6"}]}`); code != 409 {
		t.Fatalf("re-presenting a held key %d", code)
	}
	if s.operatorVersion(ada) != version {
		t.Fatal("re-presenting a held key wrote the profile")
	}
}
