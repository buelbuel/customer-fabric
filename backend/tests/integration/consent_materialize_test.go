package integration

import (
	"context"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// TestWithdrawalMaterializesAcrossActiveAssociation proves a source-reported withdrawal lands on the
// customer side of an active association, so a later unlink cannot revive a permission (ADR 0009 section 3).
func TestWithdrawalMaterializesAcrossActiveAssociation(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("newsletter")
	ctx := context.Background()

	// Build a record epoch (external identifier) and an active association to a customer subject.
	recordProfile := s.seedProfile()
	customerProfile := s.seedProfile()
	var epoch string
	if err := s.pool.QueryRow(ctx, `INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES('console','crm',$1,$2) RETURNING record_epoch`, "synthetic-mat-1", recordProfile).Scan(&epoch); err != nil {
		t.Fatal(err)
	}
	// customer_identity_links requires a Kratos identity row; the association FK points at it.
	kratos := platform.UUID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO customer_identity_links(kratos_identity_id,profile_id) VALUES($1,$2)`, kratos, customerProfile); err != nil {
		t.Fatal(err)
	}
	grant := platform.UUID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO linking_grants(id,secret_hash,source,record_profile_id,record_epoch,customer_subject,proof_method,proof_reference,issued_at,expires_at)
		VALUES($1,decode(repeat('ab',32),'hex'),'console',$2,$3,$4,'synthetic_challenge',$5,now(),now()+interval '1 hour')`,
		grant, recordProfile, epoch, kratos, platform.UUID()); err != nil {
		t.Fatal(err)
	}
	assoc := platform.UUID()
	if _, err := s.pool.Exec(ctx, `INSERT INTO verified_associations(id,grant_id,customer_subject,source,record_profile_id,record_epoch)
		VALUES($1,$2,$3,'console',$4,$5)`, assoc, grant, kratos, recordProfile, epoch); err != nil {
		t.Fatal(err)
	}

	// A source-reported withdrawal about the record.
	body := s.evidenceBody(policy, "withdraw", "source")
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/record/"+epoch, body, operatorToken, "evt-mat-0001"), 201)

	// The customer side now carries a materialized withdrawal evidence row.
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM consent_evidence_materializations m
		JOIN consent_subjects cs ON cs.id=m.subject_id WHERE cs.kind='customer' AND cs.anchor_profile_id=$1`, customerProfile).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("expected 1 materialized withdrawal on the customer side, got %d", n)
	}

	// Revoke the association; the materialized withdrawal persists, so permission cannot revive.
	if _, err := s.pool.Exec(ctx, `UPDATE verified_associations SET revoked_at=now(), revoked_by='customer' WHERE id=$1`, assoc); err != nil {
		t.Fatal(err)
	}
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM consent_evidence_materializations m
		JOIN consent_subjects cs ON cs.id=m.subject_id WHERE cs.kind='customer' AND cs.anchor_profile_id=$1`, customerProfile).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("materialized withdrawal lost after unlink, got %d", n)
	}
}

// TestSubjectLifecycleRejectsErasedWrites proves an erased subject is a write fence, not a silent accept.
func TestSubjectLifecycleRejectsErasedWrites(t *testing.T) {
	s := consentSetup(t)
	policy := s.seedActivePolicy("profiling")
	profile := s.seedProfile()
	// Pre-create an erased customer subject directly to model a fenced subject.
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO consent_subjects(id,kind,lifecycle_state,anchor_profile_id) VALUES($1,'customer','erased',$2)`, platform.UUID(), profile); err != nil {
		t.Fatal(err)
	}
	s.expect(s.callHeader("POST", "/v1/operator/consent/evidence/customer/"+profile, s.evidenceBody(policy, "grant", "customer"), operatorToken, "evt-erased-001"), 403)
}

var _ = time.Now
