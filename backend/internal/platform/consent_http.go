package platform

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
)

const purposePath = "/v1/operator/consent/purposes"
const policyPath = "/v1/operator/consent/policies"

type purposeRequest struct {
	Name string `json:"name"`
}
type policyDraftRequest struct {
	LawfulBasis        string `json:"lawful_basis"`
	NoticeReference    string `json:"notice_reference"`
	Jurisdiction       string `json:"jurisdiction"`
	Locale             string `json:"locale"`
	Channel            string `json:"channel"`
	RetentionReference string `json:"retention_reference"`
	CarryForward       bool   `json:"carry_forward"`
}
type policyTransitionRequest struct {
	State string `json:"state"`
}

// serveConsent routes the operator consent-registry endpoints. All are operator-authenticated and
// never touch erasure, holds, restore or the recovery journal (ADR 0009 sections 4 and 5).
func (a *API) serveConsent(w http.ResponseWriter, r *http.Request) {
	if _, err := a.authorizeOperator(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		return
	}
	ctx := r.Context()
	switch {
	case r.URL.Path == purposePath && r.Method == http.MethodPost:
		var body purposeRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		p, err := a.Store.CreatePurpose(ctx, body.Name)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, p)
	case strings.HasPrefix(r.URL.Path, policyPath) && r.Method == http.MethodGet:
		rest := strings.TrimPrefix(r.URL.Path, policyPath+"/")
		if !validID(rest) {
			writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
			return
		}
		versions, err := a.Store.ListPolicyVersions(ctx, rest)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": versions})
	case strings.HasPrefix(r.URL.Path, policyPath) && r.Method == http.MethodPost:
		rest := strings.TrimPrefix(r.URL.Path, policyPath+"/")
		if !validID(rest) {
			writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
			return
		}
		var body policyDraftRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		v, err := a.Store.DraftPolicyVersion(ctx, rest, consent.PolicyVersion{
			LawfulBasis: consent.LawfulBasis(body.LawfulBasis), NoticeReference: body.NoticeReference,
			Jurisdiction: body.Jurisdiction, Locale: body.Locale, Channel: body.Channel,
			RetentionReference: body.RetentionReference, CarryForward: body.CarryForward,
		})
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, v)
	default:
		http.NotFound(w, r)
	}
}

// servePolicyTransition applies a reviewable state change (draft->active, active->retired).
func (a *API) servePolicyTransition(w http.ResponseWriter, r *http.Request) {
	if _, err := a.authorizeOperator(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		return
	}
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, policyPath+"/"), "/state")
	if !ok || !validID(id) || r.Method != http.MethodPost {
		writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
		return
	}
	var body policyTransitionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	v, err := a.Store.TransitionPolicyVersion(r.Context(), id, body.State)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, v)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Header.Get("Content-Type") != "application/json" {
		writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
		return false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeProblem(w, problem(400, "Invalid request", "The request body is not valid JSON for this endpoint."))
		return false
	}
	return true
}

func validID(s string) bool {
	return len(s) == 36 && !strings.ContainsAny(s, "/ ")
}
