package platform

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const consentPrefix = "/v1/operator/consent/"
const purposePath = consentPrefix + "purposes"
const policyPath = consentPrefix + "policies/"
const policyVersionPath = consentPrefix + "policy-versions/"

type purposeRequest struct {
	Name string `json:"name"`
}
type policyDraftRequest struct {
	LawfulBasis        string `json:"lawful_basis"`
	NoticeReference    string `json:"notice_reference"`
	Explanation        string `json:"explanation"`
	Jurisdiction       string `json:"jurisdiction"`
	Locale             string `json:"locale"`
	Channel            string `json:"channel"`
	RetentionReference string `json:"retention_reference"`
	CarryForward       bool   `json:"carry_forward"`
}
type policyTransitionRequest struct {
	State string `json:"state"`
}

// authorizeConsent authenticates an operator with the same error semantics as the M1 operator routes.
// Writes are refused for Kratos operator sessions: the only operator role M1 defines is the read-only
// profile_reader (ADR 0008), and no consent write role exists yet. Development token mode may write.
func (a *API) authorizeConsent(w http.ResponseWriter, r *http.Request, write bool) bool {
	if _, err := a.authorizeOperator(r); err != nil {
		switch {
		case errors.Is(err, identity.ErrOperatorPermission):
			writeProblem(w, problem(403, "Forbidden", "Operator MFA and an operator role are required."))
		case errors.Is(err, identity.ErrUnavailable):
			writeProblem(w, problem(503, "Identity service unavailable", "Operator authentication is temporarily unavailable."))
		default:
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		}
		return false
	}
	if write && a.operatorKratos != nil {
		writeProblem(w, problem(403, "Forbidden", "Consent writes require an operator write role, which is not defined yet."))
		return false
	}
	return true
}

// serveConsent routes every operator consent endpoint. None of them touches erasure, holds, restore
// or the recovery journal (ADR 0009 sections 4 and 5).
func (a *API) serveConsent(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	switch {
	case strings.HasPrefix(path, evidenceCustomerPath) || strings.HasPrefix(path, evidenceRecordPath):
		a.serveConsentEvidence(w, r)
	case strings.HasPrefix(path, evaluatePath):
		a.serveEffectivePermission(w, r)
	case path == purposePath:
		if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, true) {
			return
		}
		a.createPurpose(w, r)
	case strings.HasPrefix(path, policyVersionPath):
		id, ok := strings.CutSuffix(strings.TrimPrefix(path, policyVersionPath), "/state")
		if !ok || !profiles.ID.MatchString(id) {
			writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
			return
		}
		if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, true) {
			return
		}
		a.transitionPolicy(w, r, id)
	case strings.HasPrefix(path, policyPath):
		purposeID := strings.TrimPrefix(path, policyPath)
		if !profiles.ID.MatchString(purposeID) {
			writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
			return
		}
		if !allow(w, r, http.MethodGet, http.MethodPost) || !a.authorizeConsent(w, r, r.Method == http.MethodPost) {
			return
		}
		if r.Method == http.MethodGet {
			a.listPolicies(w, r, purposeID)
		} else {
			a.draftPolicy(w, r, purposeID)
		}
	default:
		writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
	}
}

func (a *API) createPurpose(w http.ResponseWriter, r *http.Request) {
	var body purposeRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p, err := a.Store.CreatePurpose(r.Context(), body.Name)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, p)
}

func (a *API) listPolicies(w http.ResponseWriter, r *http.Request, purposeID string) {
	versions, err := a.Store.ListPolicyVersions(r.Context(), purposeID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": versions})
}

func (a *API) draftPolicy(w http.ResponseWriter, r *http.Request, purposeID string) {
	var body policyDraftRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	v, err := a.Store.DraftPolicyVersion(r.Context(), purposeID, consent.PolicyVersion{
		LawfulBasis: consent.LawfulBasis(body.LawfulBasis), NoticeReference: body.NoticeReference, Explanation: body.Explanation,
		Jurisdiction: body.Jurisdiction, Locale: body.Locale, Channel: body.Channel,
		RetentionReference: body.RetentionReference, CarryForward: body.CarryForward,
	})
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, v)
}

func (a *API) transitionPolicy(w http.ResponseWriter, r *http.Request, id string) {
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

func allow(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
	return false
}

// decodeJSON accepts exactly one JSON object with known fields, using the same media-type parsing as
// the M1 routes, and rejects trailing data.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		writeProblem(w, problem(413, "Payload too large", "Request body must not exceed 16 KiB."))
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(v) != nil || dec.Decode(&struct{}{}) != io.EOF || duplicateKeys(raw) {
		writeProblem(w, problem(400, "Invalid request", "Supply one JSON object with only the supported fields."))
		return false
	}
	return true
}
