package platform

import (
	"net/http"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const evidenceCustomerPath = consentPrefix + "evidence/customer/"
const evidenceRecordPath = consentPrefix + "evidence/record/"

// evidenceRequest has no actor or source field: the route decides who is speaking. An operator
// can record decisions, but never one that counts as an authenticated customer grant.
type evidenceRequest struct {
	PolicyVersionID string `json:"policy_version_id"`
	Decision        string `json:"decision"`
	OccurredAt      string `json:"occurred_at"`
	ProofChannel    string `json:"proof_channel"`
	ProofReference  string `json:"proof_reference"`
}

// serveConsentEvidence records append-only operator decisions about a customer anchor or a source
// record epoch. Idempotency-Key makes a write idempotent per subject. Nothing here is irreversible.
func (a *API) serveConsentEvidence(w http.ResponseWriter, r *http.Request) {
	customer := strings.HasPrefix(r.URL.Path, evidenceCustomerPath)
	target := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, evidenceCustomerPath), evidenceRecordPath)
	if !profiles.ID.MatchString(target) {
		writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
		return
	}
	if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, true) {
		return
	}
	key := r.Header.Values("Idempotency-Key")
	if len(key) != 1 {
		writeProblem(w, problem(422, "Invalid evidence", "Exactly one Idempotency-Key header is required."))
		return
	}
	var body evidenceRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	at, err := time.Parse(time.RFC3339Nano, body.OccurredAt)
	if err != nil || !profiles.ID.MatchString(body.PolicyVersionID) {
		writeProblem(w, problem(422, "Invalid evidence", "policy_version_id must be a version ID and occurred_at RFC 3339."))
		return
	}
	app := consent.Append{
		Decision: consent.Decision(body.Decision), ActorKind: "operator", ProofChannel: body.ProofChannel,
		ProofReference: body.ProofReference, OccurredAt: at, IdempotencyKey: key[0],
	}
	var receipt EvidenceReceipt
	if customer {
		receipt, err = a.Store.AppendCustomerEvidence(r.Context(), target, body.PolicyVersionID, app)
	} else {
		receipt, err = a.Store.AppendRecordEvidence(r.Context(), target, body.PolicyVersionID, app)
	}
	if err != nil {
		fail(w, err)
		return
	}
	status := 200
	if receipt.Created {
		status = 201
	}
	writeJSON(w, status, receipt)
}
