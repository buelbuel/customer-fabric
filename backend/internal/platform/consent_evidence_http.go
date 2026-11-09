package platform

import (
	"net/http"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
)

const evidenceCustomerPath = "/v1/operator/consent/evidence/customer/"
const evidenceRecordPath = "/v1/operator/consent/evidence/record/"

type evidenceRequest struct {
	PolicyVersionID string `json:"policy_version_id"`
	Decision        string `json:"decision"`
	OccurredAt      string `json:"occurred_at"`
	ProofChannel    string `json:"proof_channel"`
	ProofReference  string `json:"proof_reference"`
	ActorKind       string `json:"actor_kind"`
	Source          string `json:"source,omitempty"`
}

func (r evidenceRequest) append() (consent.Append, error) {
	at, err := time.Parse(time.RFC3339, r.OccurredAt)
	if err != nil {
		return consent.Append{}, err
	}
	return consent.Append{
		Decision: consent.Decision(r.Decision), ActorKind: r.ActorKind, ProofChannel: r.ProofChannel,
		ProofReference: r.ProofReference, OccurredAt: at, IdempotencyKey: "", Source: r.Source,
	}, nil
}

// serveConsentEvidence records append-only decisions about a customer profile or a source record epoch.
// The Idempotency-Key header makes a write idempotent per subject. Nothing here is irreversible.
func (a *API) serveConsentEvidence(w http.ResponseWriter, r *http.Request) {
	if _, err := a.authorizeOperator(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	key, hasKey := r.Header["Idempotency-Key"]
	if !hasKey || len(key) != 1 {
		writeProblem(w, problem(422, "Invalid evidence", "Exactly one Idempotency-Key header is required."))
		return
	}
	var body evidenceRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	app, err := body.append()
	if err != nil {
		writeProblem(w, problem(422, "Invalid evidence", "occurred_at must be RFC 3339."))
		return
	}
	app.IdempotencyKey = key[0]
	ctx := r.Context()
	var subject consent.Subject
	var created bool
	switch {
	case strings.HasPrefix(r.URL.Path, evidenceCustomerPath):
		profileID := strings.TrimPrefix(r.URL.Path, evidenceCustomerPath)
		if !validID(profileID) {
			writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
			return
		}
		subject, created, err = a.Store.AppendCustomerEvidence(ctx, profileID, body.PolicyVersionID, app)
	case strings.HasPrefix(r.URL.Path, evidenceRecordPath):
		epoch := strings.TrimPrefix(r.URL.Path, evidenceRecordPath)
		if !validID(epoch) {
			writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
			return
		}
		subject, created, err = a.Store.AppendRecordEvidence(ctx, epoch, body.PolicyVersionID, app)
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fail(w, err)
		return
	}
	status := 200
	if created {
		status = 201
	}
	writeJSON(w, status, map[string]any{"subject_id": subject.ID, "created": created})
}
