package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// audienceFixture is a synthetic customer population written set-based, so tests and benchmarks can
// reach sizes that per-request setup cannot. Customer g (1..n) is shaped by g alone:
//   - one console record with tier gold when g%4 == 0, else silver, plus an undefined attribute and a
//     governed attribute the segmentation reader may not read;
//   - a conflicting importer record with tier silver when g%8 == 4;
//   - a restricted console record when g%50 == 0;
//   - a consent grant when g%3 != 0, withdrawn later when g%10 == 0;
//   - an erasure-pending customer subject when g%97 == 0.
//
// Customers the suite already provisioned have no consent and count as consent exclusions.
type audienceFixture struct {
	n, existing int
	purpose     string
}

// population is every customer anchor, including those the suite provisioned before the fixture.
func (f audienceFixture) population() int { return f.n + f.existing }

func (f audienceFixture) member(g int) bool {
	return g%4 == 0 && g%3 != 0 && g%10 != 0 && g%97 != 0 && g%50 != 0
}

// expected returns the preview counts the rules above imply.
func (f audienceFixture) expected() (members int, excluded platform.SegmentExcluded) {
	excluded.Consent = f.existing
	for g := 1; g <= f.n; g++ {
		switch {
		case g%97 == 0:
			excluded.Unavailable++
		case g%3 == 0 || g%10 == 0:
			excluded.Consent++
		case f.member(g):
			members++
		default:
			excluded.Criteria++
		}
	}
	return members, excluded
}

// buildAudience governs tier and notes, creates the purpose and n customers, and defines the segment
// gold: tier equals gold under the purpose.
func buildAudience(s linkingSuite, n int) audienceFixture {
	s.t.Helper()
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation","activation"],"sources":["console","importer"]}`)
	s.govern("notes", `{"type":"string"}`, `{"readers":["operator"],"sources":["console"]}`)
	purpose, policy := s.consentPurpose("audience_scale")
	existing := s.count("customer_identity_links")
	ctx := context.Background()
	steps := []string{
		`CREATE TEMP TABLE fx ON COMMIT DROP AS SELECT g, gen_random_uuid() AS anchor, gen_random_uuid() AS kratos,
		   gen_random_uuid() AS rec, gen_random_uuid() AS epoch, gen_random_uuid() AS mut, gen_random_uuid() AS grant_id,
		   gen_random_uuid() AS rec2, gen_random_uuid() AS epoch2, gen_random_uuid() AS mut2, gen_random_uuid() AS grant2,
		   CASE WHEN g % 4 = 0 THEN 'gold' ELSE 'silver' END AS tier FROM generate_series(1, $1::int) g`,
		`INSERT INTO profiles(id, version, attributes) SELECT anchor, 1, '{}' FROM fx`,
		`INSERT INTO customer_identity_links(kratos_identity_id, profile_id) SELECT kratos, anchor FROM fx`,
		`INSERT INTO profiles(id, version, attributes, creator_source, restricted)
		   SELECT rec, 1, jsonb_build_object('tier', tier, 'legacy_code', 'L' || g, 'notes', 'synthetic'), 'console', g % 50 = 0 FROM fx
		   UNION ALL SELECT rec2, 1, '{"tier":"silver"}'::jsonb, 'importer', false FROM fx WHERE g % 8 = 4`,
		`INSERT INTO external_identifiers(source, namespace, value, profile_id, record_epoch)
		   SELECT 'console', 'crm', 'fx-' || g, rec, epoch FROM fx
		   UNION ALL SELECT 'importer', 'crm', 'fx-' || g, rec2, epoch2 FROM fx WHERE g % 8 = 4`,
		`INSERT INTO mutations(id, source, key, request_hash, profile_id, status, result)
		   SELECT mut, 'console', 'fixture-console-' || g, 'fixture', rec, 201, '{}'::jsonb FROM fx
		   UNION ALL SELECT mut2, 'importer', 'fixture-importer-' || g, 'fixture', rec2, 201, '{}'::jsonb FROM fx WHERE g % 8 = 4`,
		`INSERT INTO attribute_contributions(profile_id, path, source)
		   SELECT rec, path, 'console' FROM fx, unnest(ARRAY['tier','legacy_code','notes']) path
		   UNION ALL SELECT rec2, 'tier', 'importer' FROM fx WHERE g % 8 = 4`,
		`INSERT INTO provenance(profile_id, path, source, mutation_id, outcome, rule, attribute_revision)
		   SELECT rec, path, 'console', mut, 'accepted', 'source-record', (SELECT max(revision) FROM attribute_definitions d WHERE d.path = p.path)
		   FROM fx, unnest(ARRAY['tier','legacy_code','notes']) p(path)
		   UNION ALL SELECT rec2, 'tier', 'importer', mut2, 'accepted', 'source-record', (SELECT max(revision) FROM attribute_definitions WHERE path = 'tier')
		   FROM fx WHERE g % 8 = 4`,
		`INSERT INTO linking_grants(id, secret_hash, source, record_profile_id, record_epoch, customer_subject, proof_method, proof_reference, issued_at, expires_at, consumed_at)
		   SELECT grant_id, sha256(('console' || grant_id)::bytea), 'console', rec, epoch, kratos, 'synthetic_challenge', gen_random_uuid(), now() - interval '1 hour', now(), now() FROM fx
		   UNION ALL SELECT grant2, sha256(('importer' || grant2)::bytea), 'importer', rec2, epoch2, kratos, 'synthetic_challenge', gen_random_uuid(), now() - interval '1 hour', now(), now() FROM fx WHERE g % 8 = 4`,
		`INSERT INTO verified_associations(id, grant_id, customer_subject, source, record_profile_id, record_epoch)
		   SELECT gen_random_uuid(), grant_id, kratos, 'console', rec, epoch FROM fx
		   UNION ALL SELECT gen_random_uuid(), grant2, kratos, 'importer', rec2, epoch2 FROM fx WHERE g % 8 = 4`,
		`INSERT INTO consent_subjects(id, kind, anchor_profile_id, lifecycle_state)
		   SELECT gen_random_uuid(), 'customer', anchor, CASE WHEN g % 97 = 0 THEN 'erasure_pending' ELSE 'active' END FROM fx WHERE g % 3 <> 0 OR g % 97 = 0`,
		`INSERT INTO consent_evidence(id, subject_id, policy_version_id, decision, actor_kind, occurred_at, proof_channel, proof_reference, idempotency_key)
		   SELECT gen_random_uuid(), s.id, $1::uuid, 'grant', 'customer', now(), 'fixture', 'fixture', 'fixture-grant-' || g
		   FROM fx JOIN consent_subjects s ON s.kind = 'customer' AND s.anchor_profile_id = fx.anchor WHERE g % 3 <> 0`,
		`INSERT INTO consent_evidence(id, subject_id, policy_version_id, decision, actor_kind, occurred_at, proof_channel, proof_reference, idempotency_key)
		   SELECT gen_random_uuid(), s.id, $1::uuid, 'withdraw', 'customer', now(), 'fixture', 'fixture', 'fixture-withdraw-' || g
		   FROM fx JOIN consent_subjects s ON s.kind = 'customer' AND s.anchor_profile_id = fx.anchor WHERE g % 3 <> 0 AND g % 10 = 0`,
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for i, step := range steps {
		args := []any{}
		switch {
		case i == 0:
			args = append(args, n)
		case len(steps)-i <= 2:
			args = append(args, policy)
		}
		if _, err = tx.Exec(ctx, step, args...); err != nil {
			s.t.Fatalf("fixture step %d: %v", i, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		s.t.Fatal(err)
	}
	if _, err = s.pool.Exec(ctx, `ANALYZE`); err != nil {
		s.t.Fatal(err)
	}
	s.expect(s.request("POST", segmentsPath+"/gold", fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"eq","value":"gold"}]}`, purpose), "", viewOperator, ""), 201)
	return audienceFixture{n: n, existing: existing, purpose: purpose}
}

// Audiences have no population cap: a population spanning several evaluation chunks is decided
// completely and identically by preview, snapshot and activation; small audiences among many nonmembers
// page without gaps; and every list of customers is paged instead of truncated or refused.
func TestAudiencesScalePastOneThousandCustomers(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	f := buildAudience(s.linkingSuite, 2500)
	members, excluded := f.expected()

	first, _ := s.preview("gold", "?limit=100")
	if first.Population != f.population() || first.Members != members || platform.SegmentExcluded(first.Excluded) != excluded {
		t.Fatalf("preview %d/%d %+v, want %d/%d %+v", first.Members, first.Population, first.Excluded, members, f.population(), excluded)
	}
	var paged []string
	for cursor := ""; ; {
		query := "?limit=37"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page, _ := s.preview("gold", query)
		for _, m := range page.Sample {
			paged = append(paged, m.AnchorProfileID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if len(paged) != members || !slices.IsSorted(paged) || len(slices.Compact(slices.Clone(paged))) != members {
		t.Fatalf("paged %d members, want %d distinct in order", len(paged), members)
	}

	snap := s.snapshot("gold")
	if snap.Population != f.population() || snap.Members != members || snap.Excluded != excluded {
		t.Fatalf("snapshot %+v", snap)
	}

	s.destination("scale", r)
	activation := s.activate("gold", "scale")
	if activation.Queued != members || activation.Population != f.population() {
		t.Fatalf("activation %+v", activation)
	}
	var delivered []string
	for cursor := ""; ; {
		path := "/v1/operator/activations/" + activation.ID + "?limit=50"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		var page platform.ActivationStatus
		res := s.request("GET", path, "", "", viewOperator, "")
		s.expect(res, 200)
		if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
			t.Fatal(err)
		}
		if page.Counts["pending"] != members {
			t.Fatalf("counts %+v", page.Counts)
		}
		for _, d := range page.Deliveries {
			delivered = append(delivered, d.AnchorProfileID)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(delivered, paged) {
		t.Fatalf("activation pages %d deliveries, preview %d members", len(delivered), len(paged))
	}

	// Withdrawing three members shows as exactly three leaving, paged one at a time.
	for _, anchor := range paged[:3] {
		if _, err := s.pool.Exec(context.Background(), `INSERT INTO consent_evidence(id, subject_id, policy_version_id, decision, actor_kind, occurred_at, proof_channel, proof_reference, idempotency_key)
			SELECT gen_random_uuid(), s.id, e.policy_version_id, 'withdraw', 'customer', now(), 'fixture', 'fixture', 'fixture-late-' || s.id
			FROM consent_subjects s JOIN consent_evidence e ON e.subject_id = s.id WHERE s.kind = 'customer' AND s.anchor_profile_id = $1 LIMIT 1`, anchor); err != nil {
			t.Fatal(err)
		}
	}
	later := s.snapshot("gold")
	var left []string
	for cursor := ""; ; {
		path := "/v1/operator/segment-snapshots/" + later.ID + "/changes?since=" + snap.ID + "&limit=1"
		if cursor != "" {
			path += "&left_cursor=" + cursor
		}
		var c platform.SnapshotChanges
		res := s.request("GET", path, "", "", viewOperator, "")
		s.expect(res, 200)
		if err := json.Unmarshal(res.Body.Bytes(), &c); err != nil {
			t.Fatal(err)
		}
		if len(c.Joined) != 0 || c.NextJoinedCursor != "" {
			t.Fatalf("joined %+v", c)
		}
		left = append(left, c.Left...)
		if cursor = c.NextLeftCursor; cursor == "" {
			break
		}
	}
	if !slices.Equal(left, paged[:3]) {
		t.Fatalf("left %v, want %v", left, paged[:3])
	}
	for _, query := range []string{"?limit=0", "?limit=1001", "?cursor=nope", "?cursor=" + paged[0] + "&cursor=" + paged[1], "?other=1"} {
		s.expect(s.request("GET", "/v1/operator/activations/"+activation.ID+query, "", "", viewOperator, ""), 400)
	}

	// An evaluation past the store's timeout is refused, never returned partial.
	hurried, err := platform.NewAPI(platform.Store{Pool: s.pool, EvaluationTimeout: time.Millisecond}, s.config)
	if err != nil {
		t.Fatal(err)
	}
	for _, req := range []struct{ method, path string }{{"GET", segmentsPath + "/gold/preview"}, {"POST", segmentsPath + "/gold/snapshots"}} {
		res := httptest.NewRecorder()
		call := httptest.NewRequest(req.method, req.path, nil)
		call.Header.Set("Authorization", "Bearer "+viewOperator)
		hurried.ServeHTTP(res, call)
		s.expect(res, 503)
	}
	if n := s.count("segment_snapshots"); n != 2 {
		t.Fatalf("a timed-out snapshot was stored: %d snapshots", n)
	}
}

// TestAudienceBenchmark measures preview, snapshot and activation over 10k and 100k synthetic customers
// through the API, under its default time limits. It is opt-in: make audience-benchmark.
func TestAudienceBenchmark(t *testing.T) {
	if os.Getenv("CF_AUDIENCE_BENCHMARK") != "1" {
		t.Skip("set CF_AUDIENCE_BENCHMARK=1 to run the 10k/100k audience benchmark")
	}
	for _, n := range []int{10_000, 100_000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r := newReceiver(t)
			s := setupActivation(t, r)
			started := time.Now()
			f := buildAudience(s.linkingSuite, n)
			t.Logf("fixture: %d customers in %s", n, time.Since(started).Round(time.Millisecond))
			members, excluded := f.expected()
			measure := func(name string, run func()) {
				started := time.Now()
				run()
				t.Logf("%s over %d customers: %s", name, n, time.Since(started).Round(time.Millisecond))
			}
			measure("preview", func() {
				p, _ := s.preview("gold", "?limit=100")
				if p.Members != members || platform.SegmentExcluded(p.Excluded) != excluded {
					t.Fatalf("preview %+v", p)
				}
			})
			measure("snapshot", func() {
				if snap := s.snapshot("gold"); snap.Members != members {
					t.Fatalf("snapshot %+v", snap)
				}
			})
			s.destination("bench", r)
			measure("activation", func() {
				if a := s.activate("gold", "bench"); a.Queued != members {
					t.Fatalf("activation %+v", a)
				}
			})
		})
	}
}
