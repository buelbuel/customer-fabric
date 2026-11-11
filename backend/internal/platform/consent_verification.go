package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	// verificationPeriod is how long a source has to answer a verification; an unanswered one voids the grant.
	verificationPeriod = 7 * 24 * time.Hour
	// proofDocumentLimit bounds a proof document.
	proofDocumentLimit = 64 << 10
	// proofSchema names the verifiable evidence contract (ADR 0017).
	proofSchema = "customer-fabric.consent-proof.v1"
	// sourceConsentPerMinute bounds the consent requests one source makes per minute.
	sourceConsentPerMinute = 120
	// maxAuditSample bounds one audit's sample.
	maxAuditSample = 100
)

// Reasons an operator can ask for a grant's proof; policy is set by an authority's verified trust policy.
var verificationReasons = []string{"audit", "dispute", "access_request"}

// Verification is a request for, and the result of, an independent check of one source grant's proof.
// State is requested, verified, failed, or overdue: a request past its deadline, which voids the grant
// like a failure.
type Verification struct {
	ID          string     `json:"id"`
	EvidenceID  string     `json:"evidence_id"`
	Reason      string     `json:"reason"`
	RequestedBy string     `json:"requested_by"`
	RequestedAt time.Time  `json:"requested_at"`
	DueAt       time.Time  `json:"due_at"`
	State       string     `json:"state"`
	Result      string     `json:"result,omitempty"`
	DecidedAt   *time.Time `json:"decided_at,omitempty"`
}

func verificationFromRow(r db.ConsentEvidenceVerification) Verification {
	v := Verification{ID: r.ID, EvidenceID: r.EvidenceID, Reason: r.Reason, RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt, DueAt: r.DueAt,
		State: r.State, Result: r.Result.String}
	if r.DecidedAt.Valid {
		v.DecidedAt = &r.DecidedAt.Time
	}
	if v.State == "requested" && time.Now().After(v.DueAt) {
		v.State = "overdue"
	}
	return v
}

// RequestVerification asks the source behind a grant for its proof, by verificationPeriod from now.
// Until it is answered the grant keeps its standing; unanswered by then, it stops counting.
func (s Store) RequestVerification(ctx context.Context, evidenceID, reason, actor string) (Verification, error) {
	if !slices.Contains(verificationReasons, reason) {
		return Verification{}, problem(422, "Invalid verification", "reason must be audit, dispute or access_request.")
	}
	q := db.New(s.Pool)
	grant, err := q.GetSourceGrant(ctx, evidenceID)
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, problem(404, "Not found", "Unknown consent evidence.")
	}
	if err != nil {
		return Verification{}, err
	}
	if grant.ActorKind != "source" || grant.Decision != "grant" || !grant.AuthorityID.Valid {
		return Verification{}, problem(422, "Invalid verification", "Only a source's grant has a proof to verify.")
	}
	row, err := q.InsertVerification(ctx, db.InsertVerificationParams{ID: UUID(), EvidenceID: evidenceID, AuthorityID: uuidString(grant.AuthorityID),
		Reason: reason, RequestedBy: actor, DueAt: time.Now().Add(verificationPeriod)})
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, problem(409, "Verification open", "This grant already has an open verification.")
	}
	if err != nil {
		return Verification{}, err
	}
	return verificationFromRow(row), nil
}

// EvidenceVerifications lists a grant's verifications, oldest first.
func (s Store) EvidenceVerifications(ctx context.Context, evidenceID string) ([]Verification, error) {
	q := db.New(s.Pool)
	if _, err := q.GetSourceGrant(ctx, evidenceID); errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Unknown consent evidence.")
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListEvidenceVerifications(ctx, evidenceID)
	out := make([]Verification, 0, len(rows))
	for _, r := range rows {
		out = append(out, verificationFromRow(r))
	}
	return out, err
}

// AuditAuthority requests verification of up to sample grants, chosen at random among the authority's
// grants never asked before. It is audited on the authority.
func (s Store) AuditAuthority(ctx context.Context, id string, sample int, actor string) ([]Verification, error) {
	if sample < 1 || sample > maxAuditSample {
		return nil, problem(422, "Invalid audit", "sample must be 1 to 100.")
	}
	out := []Verification{}
	_, err := s.changeAuthority(ctx, id, func(q *db.Queries, a db.ConsentAuthority) (db.ConsentAuthority, error) {
		grants, err := q.SampleUnverifiedGrants(ctx, db.SampleUnverifiedGrantsParams{AuthorityID: optionalUUID(a.ID), RowLimit: int32(sample)})
		if err != nil {
			return a, err
		}
		due := time.Now().Add(verificationPeriod)
		for _, evidence := range grants {
			row, err := q.InsertVerification(ctx, db.InsertVerificationParams{ID: UUID(), EvidenceID: evidence, AuthorityID: a.ID, Reason: "audit",
				RequestedBy: actor, DueAt: due})
			if err != nil {
				return a, err
			}
			out = append(out, verificationFromRow(row))
		}
		return a, auditAuthority(ctx, q, a.ID, "audit_sampled", "", "sample:"+strconv.Itoa(len(out)), actor)
	})
	return out, err
}

// OpenVerification is a verification a source must answer, named by its own receipt reference and record.
type OpenVerification struct {
	ID              string         `json:"id"`
	Reason          string         `json:"reason"`
	RequestedAt     time.Time      `json:"requested_at"`
	DueAt           time.Time      `json:"due_at"`
	Reference       string         `json:"reference"`
	PolicyVersionID string         `json:"policy_version_id"`
	Record          *ingest.Record `json:"record,omitempty"`
}

// OpenVerifications lists the verifications of the source's own grants that it must still answer.
func (s Store) OpenVerifications(ctx context.Context, source string) ([]OpenVerification, error) {
	rows, err := db.New(s.Pool).ListOpenVerificationsForSource(ctx, pgtype.Text{String: source, Valid: true})
	out := make([]OpenVerification, 0, len(rows))
	for _, r := range rows {
		v := OpenVerification{ID: r.ID, Reason: r.Reason, RequestedAt: r.RequestedAt, DueAt: r.DueAt, Reference: r.ProofReference, PolicyVersionID: r.PolicyVersionID}
		if r.Namespace.Valid {
			v.Record = &ingest.Record{Namespace: r.Namespace.String, Value: r.Value.String}
		}
		out = append(out, v)
	}
	return out, err
}

// ProofDocument is the verifiable evidence contract: what a source produces for one grant on request.
// Its SHA-256 over the exact bytes sent must be the digest the grant recorded, and every field must match
// the grant. Evidence holds the method's own proof (for example the double opt-in confirmation's
// timestamps and message reference); it must be a non-empty object.
type ProofDocument struct {
	Schema          string          `json:"schema"`
	Record          ingest.Record   `json:"record"`
	PolicyVersionID string          `json:"policy_version_id"`
	NoticeReference string          `json:"notice_reference"`
	Method          string          `json:"method"`
	CollectedAt     string          `json:"collected_at"`
	Controller      string          `json:"controller"`
	Reference       string          `json:"reference"`
	Evidence        json.RawMessage `json:"evidence"`
}

// SubmitProof checks a source's proof document for an open verification of one of its own grants. The
// document is checked and discarded; only the result and the document's digest are kept. A failure voids
// the grant and suspends the authority pending review; so does an answer after the deadline.
func (s Store) SubmitProof(ctx context.Context, source, id string, raw []byte) (Verification, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Verification{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	open, err := q.LockVerificationForSource(ctx, db.LockVerificationForSourceParams{ID: id, Source: pgtype.Text{String: source, Valid: true}})
	if errors.Is(err, pgx.ErrNoRows) {
		return Verification{}, problem(404, "Not found", "No open verification of this source's grants has this ID.")
	}
	if err != nil {
		return Verification{}, err
	}
	sum := sha256.Sum256(raw)
	result := checkProof(open, sum[:], raw)
	if time.Now().After(open.DueAt) {
		result = "unanswered"
	}
	state := "verified"
	if result != "" {
		state = "failed"
		changed, err := q.SuspendAuthorityAutomatically(ctx, open.AuthorityID)
		if err != nil {
			return Verification{}, err
		}
		if changed == 1 {
			if err = auditAuthority(ctx, q, open.AuthorityID, "suspended", "", "verification:"+open.ID, "automatic"); err != nil {
				return Verification{}, err
			}
		}
		slog.WarnContext(ctx, "consent proof verification failed; authority suspended", "source", source, "verification_id", open.ID, "result", result)
	} else {
		result = "proof_matches"
	}
	row, err := q.DecideVerification(ctx, db.DecideVerificationParams{ID: open.ID, State: state, Result: pgtype.Text{String: result, Valid: true}, DocumentDigest: sum[:]})
	if err != nil {
		return Verification{}, err
	}
	return verificationFromRow(row), tx.Commit(ctx)
}

// checkProof returns why a proof document does not prove the grant, or "" when it does.
func checkProof(open db.LockVerificationForSourceRow, digest, raw []byte) string {
	if !bytes.Equal(digest, open.ProofDigest) {
		return "digest_mismatch"
	}
	var doc ProofDocument
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&doc) != nil || dec.More() {
		return "malformed_document"
	}
	collected, timeErr := time.Parse(time.RFC3339Nano, doc.CollectedAt)
	var evidence map[string]json.RawMessage
	switch {
	case doc.Schema != proofSchema:
		return "unknown_schema"
	case !open.Namespace.Valid:
		return "subject_unavailable"
	case doc.Record.Namespace != open.Namespace.String || doc.Record.Value != open.Value.String:
		return "subject_mismatch"
	case doc.PolicyVersionID != open.PolicyVersionID:
		return "policy_mismatch"
	case doc.NoticeReference != open.NoticeReference.String:
		return "notice_mismatch"
	case doc.Method != open.ProofChannel:
		return "method_mismatch"
	case timeErr != nil || collected.Sub(open.OccurredAt).Abs() > time.Millisecond:
		return "time_mismatch"
	case doc.Controller != open.Controller:
		return "controller_mismatch"
	case doc.Reference != open.ProofReference:
		return "reference_mismatch"
	case json.Unmarshal(doc.Evidence, &evidence) != nil || len(evidence) == 0:
		return "no_method_evidence"
	}
	return ""
}

// CountSourceConsentRequest counts one consent request against the source's rate limit and reports
// whether the source is over it.
func (s Store) CountSourceConsentRequest(ctx context.Context, source string) (bool, error) {
	q := db.New(s.Pool)
	requests, err := q.CountSourceConsentRequest(ctx, source)
	if err != nil {
		return false, err
	}
	if requests == 1 {
		// Once a minute per source, old windows are dropped.
		if err := q.PruneSourceConsentRate(ctx); err != nil {
			return false, err
		}
	}
	if requests == sourceConsentPerMinute+1 {
		slog.WarnContext(ctx, "source consent rate limit reached", "source", source)
	}
	return requests > sourceConsentPerMinute, nil
}
