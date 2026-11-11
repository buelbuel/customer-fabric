package platform

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

// Collection methods a consent authority can approve. Each names how the source proves the subject's
// consent; a boolean opt-in field in a source system is not one of them.
var collectionMethods = []string{"double_opt_in", "signed_form", "recorded_web_form", "recorded_call"}

// Channels a source can report a withdrawal, denial or objection through, besides the collection methods.
// Stopping processing needs no proof of consent, so these need neither a notice nor a digest.
var withdrawalChannels = []string{"unsubscribe_link", "support_request", "source_record_update"}

// SourceConsentDecision is one consent decision a source reports about one of its own records.
type SourceConsentDecision struct {
	Record          ingest.Record `json:"record"`
	PolicyVersionID string        `json:"policy_version_id"`
	Decision        string        `json:"decision"`
	// CollectedAt is RFC 3339; the receipt signs it exactly as sent.
	CollectedAt string `json:"collected_at"`
	SourceEpoch int64  `json:"source_epoch"`
	Proof       struct {
		Method          string `json:"method"`
		Reference       string `json:"reference"`
		NoticeReference string `json:"notice_reference"`
		SHA256          string `json:"sha256"`
	} `json:"proof"`
	// Receipt is the source's Ed25519 signature over ConsentReceiptMessage, made with a key registered on
	// its consent authority. A grant needs it; other decisions ignore it.
	Receipt struct {
		KeyID     string `json:"key_id"`
		Signature string `json:"signature"`
	} `json:"receipt"`
}

// ConsentReceiptMessage is what a source signs for a consent decision (ADR 0017): the prefix
// "customer-fabric.consent-receipt.v1" followed, each after a ".", by the unpadded base64url encoding of
// the UTF-8 bytes of the source, the record's namespace and value, the policy version, the decision,
// collected_at exactly as sent, the source epoch in decimal, the proof's method, reference, notice
// reference and lowercase SHA-256 hex, and the Idempotency-Key. The encoding is the same in every
// language. It binds the signature to the source, the record, the notice and the proof, so it cannot be
// moved to another of them.
func ConsentReceiptMessage(source, key string, d SourceConsentDecision) []byte {
	message := "customer-fabric.consent-receipt.v1"
	for _, field := range []string{source, d.Record.Namespace, d.Record.Value, d.PolicyVersionID, d.Decision, d.CollectedAt,
		strconv.FormatInt(d.SourceEpoch, 10), d.Proof.Method, d.Proof.Reference, d.Proof.NoticeReference, strings.ToLower(d.Proof.SHA256), key} {
		message += "." + base64.RawURLEncoding.EncodeToString([]byte(field))
	}
	return []byte(message)
}

// sourceAttested marks a grant as the source's signed attestation: authenticated and bound, not
// independently verified evidence that consent occurred.
const sourceAttested = "source_attested"

// sourceClockSkew bounds how far in the future a source's collection time may lie.
const sourceClockSkew = 5 * time.Minute

// AppendSourceConsent records a source's consent decision about one of its own records (ADR 0015). The
// decision binds to the record's current lifetime, under the same record lock as linking, and is
// idempotent per record subject and key.
//
// A grant is refused unless it carries the source's current epoch, read under the lock a re-key waits
// for, so grants queued before a source re-keyed are refused; the source holds an active consent
// authority for the purpose; it was collected by one of the authority's methods; it names the notice the
// policy version records; its collection time lies between the version's creation and now (with
// sourceClockSkew of tolerance) and after the subject's latest withdrawal, denial or objection for the
// purpose, so a late or replayed grant never lifts a later protective decision; and it carries the
// source's receipt reference and a SHA-256 digest of the stored proof. Withdrawals, denials and
// objections are always recorded, under any epoch and with or without authority, since stopping
// processing never needs either; they name the channel they came through.
func (s Store) AppendSourceConsent(ctx context.Context, source, key string, d SourceConsentDecision) (EvidenceReceipt, error) {
	invalid := func(detail string) (EvidenceReceipt, error) {
		return EvidenceReceipt{}, problem(422, "Invalid consent decision", detail)
	}
	grant := d.Decision == string(consent.Grant)
	digest, err := hex.DecodeString(d.Proof.SHA256)
	if len(digest) == 0 {
		digest = nil
	}
	collectedAt, timeErr := time.Parse(time.RFC3339Nano, d.CollectedAt)
	signature, signatureErr := base64.StdEncoding.DecodeString(d.Receipt.Signature)
	switch {
	case !profiles.ID.MatchString(d.PolicyVersionID):
		return invalid("policy_version_id must be a policy version ID.")
	case !consent.Decision(d.Decision).Valid():
		return invalid("decision must be grant, withdraw, deny or object.")
	case grant && !slices.Contains(collectionMethods, d.Proof.Method):
		return invalid("proof.method must be double_opt_in, signed_form, recorded_web_form or recorded_call.")
	case !grant && !slices.Contains(collectionMethods, d.Proof.Method) && !slices.Contains(withdrawalChannels, d.Proof.Method):
		return invalid("proof.method must name the channel the decision came through.")
	case d.Proof.Reference == "" || len(d.Proof.Reference) > 256:
		return invalid("proof.reference must name the source's receipt (1 to 256 characters).")
	case grant && d.Proof.NoticeReference == "" || len(d.Proof.NoticeReference) > 256:
		return invalid("proof.notice_reference must name the notice the subject was shown.")
	case err != nil || (grant || digest != nil) && len(digest) != 32:
		return invalid("proof.sha256 must be the hex SHA-256 digest of the stored proof.")
	case timeErr != nil || collectedAt.After(time.Now().Add(sourceClockSkew)):
		return invalid("collected_at must be an RFC 3339 time no later than now.")
	case grant && d.SourceEpoch <= 0 || d.SourceEpoch < 0:
		return invalid("source_epoch must be the source's current epoch.")
	case grant && (!receiptKeyID.MatchString(d.Receipt.KeyID) || signatureErr != nil || len(signature) != ed25519.SignatureSize):
		return invalid("receipt must name the signing key and carry a base64 Ed25519 signature.")
	}
	decision := consent.Append{Decision: consent.Decision(d.Decision), ActorKind: "source", Source: source, ProofChannel: d.Proof.Method,
		ProofReference: d.Proof.Reference, OccurredAt: collectedAt.UTC(), IdempotencyKey: key, NoticeReference: d.Proof.NoticeReference,
		ProofDigest: digest}
	if grant {
		// Only a grant's epoch and receipt are checked, so only a grant's are kept. The signature is part of
		// the decision, so a retry must carry the same one.
		decision.SourceGeneration, decision.ReceiptSignature = d.SourceEpoch, signature
	}
	receipt, err := s.appendEvidence(ctx, d.PolicyVersionID, decision, func(q *db.Queries) (consent.Subject, error) {
		if grant {
			// A re-key waits for this shared lock, so no grant from before it commits after it.
			if err := q.LockSourceEpochShared(ctx, source); err != nil {
				return consent.Subject{}, err
			}
			epoch, err := q.GetSourceEpoch(ctx, source)
			if err != nil {
				return consent.Subject{}, err
			}
			if epoch != d.SourceEpoch {
				return consent.Subject{}, problem(409, "Source epoch mismatch", "Send this grant with the source's current epoch.")
			}
		}
		record, err := lockSourceRecord(ctx, q, source, d.Record)
		if err != nil {
			return consent.Subject{}, err
		}
		return recordSubjectFor(ctx, q, record.RecordEpoch)
	}, func(q *db.Queries, subject consent.Subject, a *consent.Append) error {
		version, err := q.GetPolicyVersion(ctx, d.PolicyVersionID)
		if err != nil {
			return err
		}
		authority, err := q.GetActiveConsentAuthority(ctx, db.GetActiveConsentAuthorityParams{Source: source, PurposeID: version.PurposeID})
		if errors.Is(err, pgx.ErrNoRows) {
			if grant {
				return problem(403, "No consent authority", "This source is not approved to report consent for this purpose.")
			}
			return nil
		}
		if err != nil {
			return err
		}
		a.AuthorityID = authority.ID
		if !grant {
			return nil
		}
		// The receipt must be signed by an active key of this authority, over exactly this decision.
		signer, err := q.GetAuthorityKey(ctx, db.GetAuthorityKeyParams{AuthorityID: authority.ID, KeyID: d.Receipt.KeyID})
		if errors.Is(err, pgx.ErrNoRows) || err == nil && signer.State != "active" {
			slog.WarnContext(ctx, "source consent receipt refused", "source", source, "reason", "key_not_active")
			return problem(403, "Receipt key not accepted", "receipt.key_id must name an active key of the source's consent authority.")
		}
		if err != nil {
			return err
		}
		if !ed25519.Verify(signer.PublicKey, ConsentReceiptMessage(source, key, d), signature) {
			slog.WarnContext(ctx, "source consent receipt refused", "source", source, "reason", "invalid_signature")
			return problem(403, "Invalid receipt signature", "receipt.signature does not verify for this decision and key.")
		}
		a.ReceiptKey = signer.ID
		protected, err := q.LatestProtectiveDecision(ctx, db.LatestProtectiveDecisionParams{SubjectID: subject.ID, PurposeID: version.PurposeID})
		if err != nil {
			return err
		}
		switch {
		case !slices.Contains(authority.CollectionMethods, d.Proof.Method):
			return problem(422, "Invalid consent decision", "The source's authority does not approve this collection method.")
		case d.Proof.NoticeReference != version.NoticeReference:
			return problem(422, "Invalid consent decision", "proof.notice_reference must be the notice the policy version records.")
		case collectedAt.Before(version.CreatedAt):
			return problem(422, "Invalid consent decision", "collected_at precedes the policy version, so the subject cannot have seen its notice.")
		case !collectedAt.After(protected):
			return problem(409, "Withdrawn since", "The subject withdrew, denied or objected after this grant was collected; only a grant collected later counts.")
		}
		return nil
	})
	if err == nil && grant {
		receipt.Trust = sourceAttested
	}
	return receipt, err
}
