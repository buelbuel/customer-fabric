package integration

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// seedActivePolicy creates a purpose with one active consent policy version and returns its id.
func (s *consentSuite) seedActivePolicy(name string) string {
	s.t.Helper()
	p := s.purpose(name)
	v := s.draft(p.ID)
	s.expect(s.transition(v.ID, "active"), 200)
	return v.ID
}

// seedProfile creates a bare profile, as a source would.
func (s *consentSuite) seedProfile() string {
	s.t.Helper()
	id := platform.UUID()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO profiles(id,version,attributes) VALUES($1,1,'{}')`, id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// customerProfile creates a customer anchor: a profile linked to a synthetic Kratos identity.
func (s *consentSuite) customerProfile() (profile, kratos string) {
	s.t.Helper()
	profile, kratos = s.seedProfile(), platform.UUID()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO customer_identity_links(kratos_identity_id,profile_id) VALUES($1,$2)`, kratos, profile); err != nil {
		s.t.Fatal(err)
	}
	return
}

// record creates a source identifier on profile (a new profile when empty) and returns its record epoch.
func (s *consentSuite) record(profile, value string) (string, string) {
	s.t.Helper()
	if profile == "" {
		profile = s.seedProfile()
	}
	var epoch string
	if err := s.pool.QueryRow(context.Background(), `INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES('console','crm',$1,$2) RETURNING record_epoch`, value, profile).Scan(&epoch); err != nil {
		s.t.Fatal(err)
	}
	return profile, epoch
}

// link creates an active verified association, as M1 redemption would, under the record lock.
func (s *consentSuite) link(kratos, recordProfile, epoch string) string {
	s.t.Helper()
	ctx := context.Background()
	grant, assoc := platform.UUID(), platform.UUID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	for _, stmt := range []struct {
		sql  string
		args []any
	}{
		{`SELECT pg_advisory_xact_lock(hashtextextended('external-association:' || $1::uuid::text, 0))`, []any{epoch}},
		{`INSERT INTO linking_grants(id,secret_hash,source,record_profile_id,record_epoch,customer_subject,proof_method,proof_reference,issued_at,expires_at)
			VALUES($1,decode(md5($6)||md5($6),'hex'),'console',$2,$3,$4,'synthetic_challenge',$5,now(),now()+interval '1 hour')`, []any{grant, recordProfile, epoch, kratos, platform.UUID(), grant}},
		{`INSERT INTO verified_associations(id,grant_id,customer_subject,source,record_profile_id,record_epoch) VALUES($1,$2,$3,'console',$4,$5)`, []any{assoc, grant, kratos, recordProfile, epoch}},
	} {
		if _, err = tx.Exec(ctx, stmt.sql, stmt.args...); err != nil {
			s.t.Fatal(err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		s.t.Fatal(err)
	}
	return assoc
}

// unlink revokes an association the way M1 does, under the record lock.
func (s *consentSuite) unlink(assoc, epoch string) {
	s.t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		s.t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('external-association:' || $1::uuid::text, 0))`, epoch); err != nil {
		s.t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `UPDATE verified_associations SET revoked_at=clock_timestamp(), revoked_by='customer' WHERE id=$1`, assoc); err != nil {
		s.t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		s.t.Fatal(err)
	}
}

func (s *consentSuite) evidenceBody(policyID, decision string) string {
	b, _ := json.Marshal(map[string]string{
		"policy_version_id": policyID, "decision": decision, "occurred_at": time.Now().UTC().Format(time.RFC3339),
		"proof_channel": "operator_console", "proof_reference": "synthetic-ref",
	})
	return string(b)
}

func (s *consentSuite) customerEvidence(profile, policy, decision, key string) receiptView {
	s.t.Helper()
	w := s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, decision), operatorToken, key)
	if w.Code != 201 && w.Code != 200 {
		s.t.Fatalf("customer %s: %d %s", decision, w.Code, w.Body.String())
	}
	var r receiptView
	_ = json.Unmarshal(w.Body.Bytes(), &r)
	return r
}

func (s *consentSuite) recordEvidence(epoch, policy, decision, key string) {
	s.t.Helper()
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/record/"+epoch, s.evidenceBody(policy, decision), operatorToken, key), 201)
}

type receiptView struct {
	SubjectID      string `json:"subject_id"`
	EvidenceID     string `json:"evidence_id"`
	Created        bool   `json:"created"`
	Acknowledgment string `json:"acknowledgment"`
}

// Operators record decisions as operators. The body cannot claim a customer or source actor.
func TestOperatorCannotChooseActor(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	profile, _ := s.customerProfile()
	forged := `{"policy_version_id":"` + policy + `","decision":"grant","occurred_at":"2026-01-01T00:00:00Z","proof_channel":"operator_console","proof_reference":"x","actor_kind":"customer"}`
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, forged, operatorToken, "forged-grant-01"), 400)
	s.customerEvidence(profile, policy, "grant", "operator-grant-01")
	var actor string
	if err := s.pool.QueryRow(context.Background(), `SELECT actor_kind FROM consent_evidence`).Scan(&actor); err != nil || actor != "operator" {
		t.Fatalf("stored actor %q, want operator (%v)", actor, err)
	}
}

// Customer subjects anchor only on customer anchors; record subjects only on live record epochs.
func TestEvidenceAnchorsAreValidated(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	for name, path := range map[string]string{
		"source profile":   "/v1/operator/consent/evidence/customer/" + s.seedProfile(),
		"unknown profile":  "/v1/operator/consent/evidence/customer/" + platform.UUID(),
		"unknown epoch":    "/v1/operator/consent/evidence/record/" + platform.UUID(),
		"malformed target": "/v1/operator/consent/evidence/customer/zzzzzzzz-zzzz-zzzz-zzzz-zzzzzzzzzzzz",
	} {
		if w := s.callHeader("POST", path, s.evidenceBody(policy, "withdraw"), operatorToken, "anchor-check-01"); w.Code != 404 {
			t.Errorf("%s: got %d, want 404: %s", name, w.Code, w.Body.String())
		}
	}
	if n := s.count("consent_subjects"); n != 0 {
		t.Fatalf("%d subjects created for invalid anchors", n)
	}
	profile, _ := s.customerProfile()
	if w := s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody("not-a-uuid", "withdraw"), operatorToken, "bad-policy-01"); w.Code != 422 {
		t.Fatalf("malformed policy id: got %d, want 422", w.Code)
	}
	if w := s.call("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, "withdraw"), operatorToken); w.Code != 422 {
		t.Fatalf("missing idempotency key: got %d, want 422", w.Code)
	}
}

// Grants need the active version. Protective decisions are accepted on retired versions; drafts accept nothing.
func TestPolicyStateGatesEvidence(t *testing.T) {
	s := consentSetup(t)
	p := s.purpose("newsletter")
	v1 := s.draft(p.ID)
	profile, _ := s.customerProfile()
	post := func(policy, decision, key string) int {
		return s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, decision), operatorToken, key).Code
	}
	if c := post(v1.ID, "withdraw", "draft-wd-0001"); c != 409 {
		t.Fatalf("withdrawal on a draft: got %d, want 409", c)
	}
	s.expect(s.transition(v1.ID, "active"), 200)
	s.expect(s.transition(v1.ID, "retired"), 200)
	for i, d := range []string{"withdraw", "deny", "object"} {
		if c := post(v1.ID, d, "retired-pr-000"+string(rune('1'+i))); c != 201 {
			t.Fatalf("%s on a retired version: got %d, want 201", d, c)
		}
	}
	if c := post(v1.ID, "grant", "retired-gr-0001"); c != 409 {
		t.Fatalf("grant on a retired version: got %d, want 409", c)
	}
}

// A replayed key returns the stored decision; the same key with a different decision is a conflict.
func TestIdempotentReplayMustMatch(t *testing.T) {
	s := consentSetup(t)
	v1, v2 := s.seedActivePolicy("newsletter"), s.seedActivePolicy("profiling")
	profile, _ := s.customerProfile()
	key := strings.Repeat("k", 128)
	// The replay sends the identical body: a body built again could carry a later occurred_at, which is
	// rightly a different decision.
	body := s.evidenceBody(v1, "withdraw")
	send := func() receiptView {
		t.Helper()
		w := s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, body, operatorToken, key)
		if w.Code != 201 && w.Code != 200 {
			t.Fatalf("customer withdraw: %d %s", w.Code, w.Body.String())
		}
		var r receiptView
		_ = json.Unmarshal(w.Body.Bytes(), &r)
		return r
	}
	first := send()
	if !first.Created || first.Acknowledgment != "pending_journal" {
		t.Fatalf("unexpected receipt %+v", first)
	}
	replay := send()
	if replay.Created || replay.EvidenceID != first.EvidenceID {
		t.Fatalf("replay %+v does not match %+v", replay, first)
	}
	for _, c := range [][2]string{{v2, "withdraw"}, {v1, "deny"}} {
		if w := s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(c[0], c[1]), operatorToken, key); w.Code != 409 {
			t.Fatalf("reused key for a different decision: got %d, want 409", w.Code)
		}
	}
	if n := s.count("consent_evidence"); n != 1 {
		t.Fatalf("%d evidence rows, want 1", n)
	}
}

func TestEvidenceIdempotentUnderConcurrency(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	profile, _ := s.customerProfile()
	body := s.evidenceBody(policy, "withdraw")
	const n = 8
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, body, operatorToken, "evt-concurrent-0001").Code
		}(i)
	}
	wg.Wait()
	created := 0
	for _, c := range codes {
		if c == 201 {
			created++
		} else if c != 200 {
			t.Fatalf("unexpected status %d during concurrent idempotent write", c)
		}
	}
	if created != 1 || s.count("consent_evidence") != 1 {
		t.Fatalf("created=%d rows=%d, want 1 and 1", created, s.count("consent_evidence"))
	}
}

// Evidence is append-only and its receipt time comes from the database clock only.
func TestEvidenceIsAppendOnlyWithServerReceiptTime(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	profile, _ := s.customerProfile()
	s.customerEvidence(profile, policy, "withdraw", "evt-append-0001")
	ctx := context.Background()
	if _, err := s.pool.Exec(ctx, `UPDATE consent_evidence SET decision='grant'`); err == nil {
		t.Fatal("evidence row was updated")
	}
	if _, err := s.pool.Exec(ctx, `DELETE FROM consent_evidence`); err == nil {
		t.Fatal("evidence row was deleted")
	}
	var subject string
	_ = s.pool.QueryRow(ctx, `SELECT subject_id FROM consent_evidence`).Scan(&subject)
	if _, err := s.pool.Exec(ctx, `INSERT INTO consent_evidence(id,subject_id,policy_version_id,decision,actor_kind,occurred_at,received_at,proof_channel,proof_reference,idempotency_key)
		VALUES($1,$2,$3,'grant','customer',now(),'2000-01-01','customer_session','x','backdated-001')`, platform.UUID(), subject, policy); err != nil {
		t.Fatal(err)
	}
	var age time.Duration
	if err := s.pool.QueryRow(ctx, `SELECT extract(epoch FROM now()-received_at)::bigint * interval '1 second' FROM consent_evidence WHERE idempotency_key='backdated-001'`).Scan(&age); err != nil || age > time.Minute {
		t.Fatalf("caller-supplied received_at was kept (age %v, %v)", age, err)
	}
}

// Association history is permanent and single-revocation; its times come from the database clock.
func TestAssociationHistoryIsPermanent(t *testing.T) {
	s := consentSetup(t)
	_, kratos := s.customerProfile()
	rp, epoch := s.record("", "history-1")
	assoc := s.link(kratos, rp, epoch)
	ctx := context.Background()
	for name, stmt := range map[string]string{
		"delete":           `DELETE FROM verified_associations WHERE id=$1`,
		"move subject":     `UPDATE verified_associations SET customer_subject=gen_random_uuid() WHERE id=$1`,
		"backdate link":    `UPDATE verified_associations SET linked_at='2000-01-01' WHERE id=$1`,
		"clear revocation": `UPDATE verified_associations SET revoked_by=NULL WHERE id=$1`,
	} {
		if _, err := s.pool.Exec(ctx, stmt, assoc); err == nil {
			t.Errorf("%s was allowed", name)
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE verified_associations SET revoked_at='2000-01-01', revoked_by='customer' WHERE id=$1`, assoc); err != nil {
		t.Fatal(err)
	}
	var ordered bool
	if err := s.pool.QueryRow(ctx, `SELECT revoked_at > linked_at FROM verified_associations WHERE id=$1`, assoc).Scan(&ordered); err != nil || !ordered {
		t.Fatalf("revocation time not taken from the database clock (%v)", err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE verified_associations SET revoked_at=now(), revoked_by='issuer' WHERE id=$1`, assoc); err == nil {
		t.Fatal("an association was revoked twice")
	}
}

// A record decision waits for the record-epoch lock that M1 link, unlink and revoke hold, so it is
// strictly ordered against association history.
func TestRecordEvidenceWaitsForLinkLock(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	_, epoch := s.record("", "lock-1")
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('external-association:' || $1::uuid::text, 0))`, epoch); err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		done <- s.callHeader("POST", "/v1/operator/consent/evidence/record/"+epoch, s.evidenceBody(policy, "withdraw"), operatorToken, "lock-wd-00001").Code
	}()
	select {
	case c := <-done:
		_ = tx.Rollback(ctx)
		t.Fatalf("record evidence did not wait for the link lock (status %d)", c)
	case <-time.After(300 * time.Millisecond):
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if c := <-done; c != 201 {
		t.Fatalf("record evidence after the lock: %d", c)
	}
}

func TestErasedSubjectRejectsWrites(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("profiling")
	profile, _ := s.customerProfile()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO consent_subjects(id,kind,lifecycle_state,anchor_profile_id) VALUES($1,'customer','erased',$2)`, platform.UUID(), profile); err != nil {
		t.Fatal(err)
	}
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, "withdraw"), operatorToken, "evt-erased-001"), 403)
}
