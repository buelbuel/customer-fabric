package integration

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// bulkSourceRecords writes n synthetic console records set-based, numbered after the first offset, each
// with a distinct email, phone and reference, owned by console with accepted provenance, as if each had
// been created through the API.
func bulkSourceRecords(s linkingSuite, offset, n int) {
	s.t.Helper()
	ctx := context.Background()
	steps := []string{
		`CREATE TEMP TABLE mx ON COMMIT DROP AS SELECT g, gen_random_uuid() AS rec, gen_random_uuid() AS mut FROM generate_series($1::int + 1, $1::int + $2::int) g`,
		`INSERT INTO profiles(id, version, attributes, creator_source)
		   SELECT rec, 1, jsonb_build_object('email', 'bulk' || g || '@example.test', 'phone', '+49 30 ' || (10000000 + g),
		     'given_name', 'Given' || g, 'family_name', 'Family' || g, 'customer_reference', 'bulk-' || g), 'console' FROM mx`,
		`INSERT INTO external_identifiers(source, namespace, value, profile_id) SELECT 'console', 'crm', 'bulk-' || g, rec FROM mx`,
		`INSERT INTO mutations(id, source, key, request_hash, profile_id, status, result)
		   SELECT mut, 'console', 'bulk-console-key-' || g, 'fixture', rec, 201, '{}'::jsonb FROM mx`,
		`INSERT INTO attribute_contributions(profile_id, path, source)
		   SELECT rec, path, 'console' FROM mx, unnest(ARRAY['email','phone','given_name','family_name','customer_reference']) path`,
		`INSERT INTO provenance(profile_id, path, source, mutation_id, outcome, rule, attribute_revision)
		   SELECT rec, p.path, 'console', mut, 'accepted', 'source-record', (SELECT max(revision) FROM attribute_definitions d WHERE d.path = p.path)
		   FROM mx, unnest(ARRAY['email','phone','given_name','family_name','customer_reference']) p(path)`,
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i, step := range steps {
		args := []any{}
		if i == 0 {
			args = append(args, offset, n)
		}
		if _, err = tx.Exec(ctx, step, args...); err != nil {
			s.t.Fatalf("bulk step %d: %v", i, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		s.t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ANALYZE`); err != nil {
		s.t.Fatal(err)
	}
}

// Match candidates have no population cap: candidates among many unrelated records are found through
// governed values only, in one snapshot, and a record that changes or leaves matching is decided by its
// committed state at the next lookup. Nothing derived from the values is stored.
func TestMatchCandidatesScalePastFiveThousandRecords(t *testing.T) {
	s := setupLinking(t)
	for _, path := range []string{"email", "given_name", "family_name"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	// Phones are granted to matching for console only; references are not granted to matching at all.
	s.govern("phone", `{"type":"string"}`, `{"readers":["matching"],"sources":["console"]}`)
	s.govern("customer_reference", `{"type":"string"}`, `{"readers":["operator"],"sources":["console","importer"]}`)
	bulkSourceRecords(s, 0, 6_000)

	ada := s.viewRecord(tokenA, "scale-ada", `{"email":"ada@example.com","given_name":"Ada","family_name":"Muster","phone":"+44 1632 960001","customer_reference":"ref-ada"}`)
	adaERP := s.viewRecord(tokenB, "scale-ada-erp", `{"email":"ADA@example.com","family_name":"MUSTER"}`)
	household := s.viewRecord(tokenB, "scale-lukas", `{"email":"ada@example.com","given_name":"Lukas","family_name":"Muster"}`)
	// Shares the phone and family name, but importer's phone is not granted to matching.
	s.viewRecord(tokenB, "scale-phone", `{"phone":"01632 960001","family_name":"Muster"}`)
	// Shares the reference and both names, but references are not granted to matching.
	s.viewRecord(tokenB, "scale-ref", `{"customer_reference":"ref-ada","given_name":"Ada","family_name":"Muster"}`)

	started := time.Now()
	c, body := s.candidates(ada)
	t.Logf("lookup over %d records: %s", c.Population, time.Since(started))
	if c.Population < 6_005 || c.Total != 2 || c.Candidates[0].ProfileID != adaERP || c.Candidates[0].Class != "probable" ||
		c.Candidates[1].ProfileID != household || c.Candidates[1].Class != "conflicting" {
		t.Fatalf("candidates %s", body)
	}
	// A bulk record changed after an earlier lookup is found at its new version.
	var renamed string
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM profiles WHERE attributes->>'customer_reference' = 'bulk-7'`).Scan(&renamed); err != nil {
		t.Fatal(err)
	}
	s.expect(s.request("PATCH", "/v1/profiles/"+renamed, `{"set":{"email":"Ada@Example.com","family_name":"Muster","given_name":"Ada"}}`, "key-renamed-0000001", tokenA, `"1"`), 200)
	c, body = s.candidates(ada)
	if c.Total != 3 || !slices.ContainsFunc(c.Candidates, func(m platform.MatchCandidate) bool { return m.ProfileID == renamed && m.Class == "probable" }) {
		t.Fatalf("after a write %s", body)
	}

	// A record that loses its email stops matching through it.
	s.expect(s.request("PATCH", "/v1/profiles/"+renamed, `{"remove":["email"]}`, "key-renamed-0000002", tokenA, `"2"`), 200)
	if c, body = s.candidates(ada); c.Total != 2 {
		t.Fatalf("after removing the email %s", body)
	}

	// A record that is no longer active leaves matching, like a restricted one.
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET lifecycle_state = 'erasure_pending' WHERE id = $1`, household); err != nil {
		t.Fatal(err)
	}
	c, body = s.candidates(ada)
	if c.Total != 1 {
		t.Fatalf("an erasure-pending record stayed matchable %s", body)
	}

	// A restricted record leaves matching.
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET restricted = true WHERE id = $1`, adaERP); err != nil {
		t.Fatal(err)
	}
	c, body = s.candidates(ada)
	if c.Total != 0 || slices.ContainsFunc(c.Candidates, func(m platform.MatchCandidate) bool { return m.ProfileID == adaERP }) {
		t.Fatalf("a restricted record stayed a candidate %s", body)
	}

	// Revoking matching's grant on email removes every candidate found through it, at the next lookup.
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET restricted = false, lifecycle_state = 'active' WHERE id = ANY($1)`, []string{adaERP, household}); err != nil {
		t.Fatal(err)
	}
	if c, body = s.candidates(ada); c.Total != 2 {
		t.Fatalf("restored records %s", body)
	}
	s.expect(s.request("POST", "/v1/operator/attributes/email/access", `{"readers":["operator"],"sources":["console","importer"]}`, "", viewOperator, `"1"`), 201)
	if c, body = s.candidates(ada); c.Total != 0 {
		t.Fatalf("a revoked attribute still proposed candidates %s", body)
	}
}

// TestMatchBenchmark measures match-candidate lookup over 10k and 100k synthetic records through the API. It is
// opt-in: make audience-benchmark.
func TestMatchBenchmark(t *testing.T) {
	if os.Getenv("CF_AUDIENCE_BENCHMARK") != "1" {
		t.Skip("set CF_AUDIENCE_BENCHMARK=1 to run the 10k/100k match benchmark")
	}
	s := setupLinking(t)
	for _, path := range []string{"email", "phone", "given_name", "family_name", "customer_reference"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	// The second round's target also matches the first round's target record.
	bulkSourceRecords(s, 0, 10_000)
	matchBenchmark(t, s, 1)
	bulkSourceRecords(s, 10_000, 90_000)
	matchBenchmark(t, s, 2)
}

func matchBenchmark(t *testing.T, s linkingSuite, want int) {
	ada := s.viewRecord(tokenA, "bench-ada-"+platform.UUID(), `{"email":"bulk42@example.test","given_name":"Given42","family_name":"Family42"}`)
	for range 3 {
		started := time.Now()
		c, body := s.candidates(ada)
		if c.Total != want {
			t.Fatalf("candidates %s", body)
		}
		t.Logf("match lookup over %d records: %s", c.Population, time.Since(started).Round(time.Millisecond))
	}
}

// Phones and references find candidates through their own keys when matching may read them.
func TestMatchCandidatesThroughPhoneAndReferenceKeys(t *testing.T) {
	s := setupLinking(t)
	for _, path := range []string{"email", "phone", "given_name", "family_name", "customer_reference"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	bulkSourceRecords(s, 0, 6_000)
	ada := s.viewRecord(tokenA, "keys-ada", `{"given_name":"Ada","family_name":"Muster","phone":"+44 1632 960001","customer_reference":"ref-ada"}`)
	byPhone := s.viewRecord(tokenB, "keys-phone", `{"phone":"01632 960001","family_name":"MUSTER"}`)
	byReference := s.viewRecord(tokenB, "keys-ref", `{"customer_reference":"ref-ada","given_name":"Ada"}`)
	c, body := s.candidates(ada)
	if c.Total != 2 || !slices.ContainsFunc(c.Candidates, func(m platform.MatchCandidate) bool { return m.ProfileID == byPhone && m.Class == "probable" }) ||
		!slices.ContainsFunc(c.Candidates, func(m platform.MatchCandidate) bool { return m.ProfileID == byReference && m.Class == "strong" }) {
		t.Fatalf("candidates %s", body)
	}

	// A value the record's own source does not own proposes nothing. The API refuses another source's
	// write to a record, so the ownership row is changed directly.
	foreign := s.viewRecord(tokenA, "keys-foreign", `{"customer_reference":"ref-ada","given_name":"Ada"}`)
	if _, err := s.pool.Exec(context.Background(), `UPDATE attribute_contributions SET source = 'importer' WHERE profile_id = $1 AND path = 'customer_reference'`, foreign); err != nil {
		t.Fatal(err)
	}
	// A customer anchor is never a source record, whatever it holds, and an erased record takes no part.
	anchor := s.anchor(linkCustomerA)
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET attributes = '{"customer_reference":"ref-ada","given_name":"Ada"}' WHERE id = $1`, anchor); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET lifecycle_state = 'erased' WHERE id = $1`, byReference); err != nil {
		t.Fatal(err)
	}
	c, body = s.candidates(ada)
	if c.Total != 1 || c.Candidates[0].ProfileID != byPhone {
		t.Fatalf("a foreign value, an anchor or an erased record became a candidate %s", body)
	}
	for _, candidate := range []string{foreign, anchor, byReference} {
		s.expect(s.request("POST", "/v1/operator/profiles/"+ada+"/match-reviews", fmt.Sprintf(`{"candidate_profile_id":%q,"decision":"same_person"}`, candidate), "", viewOperator, ""), 409)
	}
	s.expect(s.request("POST", "/v1/operator/profiles/"+ada+"/match-reviews", fmt.Sprintf(`{"candidate_profile_id":%q,"decision":"same_person"}`, byPhone), "", viewOperator, ""), 201)
}

// The list keeps the first 100 candidates by class and ID and counts the rest, however many records
// share an identifier with the target.
func TestMatchCandidateListIsBounded(t *testing.T) {
	s := setupLinking(t)
	for _, path := range []string{"email", "phone", "given_name", "family_name", "customer_reference"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	bulkSourceRecords(s, 0, 6_000)
	// Every bulk record now shares one reference and family name with the target.
	if _, err := s.pool.Exec(context.Background(), `UPDATE profiles SET attributes = attributes || '{"customer_reference":"shared","family_name":"Muster"}', version = version WHERE attributes->>'customer_reference' LIKE 'bulk-%'`); err != nil {
		t.Fatal(err)
	}
	ada := s.viewRecord(tokenB, "bound-ada", `{"customer_reference":"shared","family_name":"Muster"}`)
	c, body := s.candidates(ada)
	if c.Total != 6_000 || len(c.Candidates) != 100 || !slices.IsSortedFunc(c.Candidates, func(a, b platform.MatchCandidate) int { return strings.Compare(a.ProfileID, b.ProfileID) }) {
		t.Fatalf("total %d, listed %d: %.200s", c.Total, len(c.Candidates), body)
	}
	var first string
	if err := s.pool.QueryRow(context.Background(), `SELECT min(id::text) FROM profiles WHERE attributes->>'customer_reference' = 'shared' AND id <> $1`, ada).Scan(&first); err != nil || c.Candidates[0].ProfileID != first {
		t.Fatalf("the list does not start at the lowest ID: %v", err)
	}
}

// A lookup decides from one committed snapshot: writes racing it never produce an error or a half-applied
// result, a replayed or out-of-order source event changes nothing, and the last committed write is what
// the next lookup shows.
func TestMatchCandidatesFollowCommittedStateUnderConcurrentWrites(t *testing.T) {
	s := setupLinking(t)
	for _, path := range []string{"email", "given_name", "family_name"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["matching"],"sources":["console","importer"]}`)
	}
	bulkSourceRecords(s, 0, 6_000)
	ada := s.viewRecord(tokenB, "race-ada", `{"email":"ada@example.com","family_name":"Muster"}`)
	other := s.viewRecord(tokenA, "race-other", `{"email":"other@example.com","family_name":"Muster"}`)
	if c, body := s.candidates(ada); c.Total != 0 {
		t.Fatalf("before any write %s", body)
	}

	const writes = 40
	failed := make(chan int, 1)
	go func() {
		defer close(failed)
		// Sequence 1 is left unused for the stale event below.
		for sequence := 2; sequence <= writes+1; sequence++ {
			email := "other@example.com"
			if sequence%2 == 1 {
				email = "ADA@example.com"
			}
			if code := s.event("race-other", sequence, fmt.Sprintf(`{"email":%q}`, email)); code != 201 {
				failed <- code
				return
			}
		}
	}()
	for done := false; !done; {
		select {
		case code, open := <-failed:
			if open {
				t.Fatalf("a concurrent source event was refused with %d", code)
			}
			done = true
		default:
		}
		c, body := s.candidates(ada)
		if c.Total > 1 || c.Total == 1 && (c.Candidates[0].ProfileID != other || c.Candidates[0].Class != "probable") {
			t.Fatalf("a lookup racing writes %s", body)
		}
	}
	// The last write (an odd sequence) made the emails agree.
	if c, body := s.candidates(ada); c.Total != 1 || c.Candidates[0].ProfileID != other {
		t.Fatalf("after the last write %s", body)
	}
	// A replay of the last event and a new event older than the record's watermark, which would remove
	// the match, change nothing.
	if s.event("race-other", writes+1, `{"email":"ADA@example.com"}`) != 200 || s.event("race-other", 1, `{"email":"other@example.com"}`) != 201 {
		t.Fatal("replayed and stale events")
	}
	if c, body := s.candidates(ada); c.Total != 1 {
		t.Fatalf("a replayed or stale event changed the candidates %s", body)
	}
}
