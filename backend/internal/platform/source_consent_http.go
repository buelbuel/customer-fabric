package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	sourceConsentPath            = "/v1/source-consent-decisions"
	operatorConsentAuthorityPath = "/v1/operator/consent-authorities"
)

// serveSourceConsent records a consent decision the authenticated source reports about one of its own
// records (ADR 0015). The source comes from the credential, never from the body.
func (a *API) serveSourceConsent(w http.ResponseWriter, r *http.Request, source string) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	key := r.Header.Values("Idempotency-Key")
	if len(key) != 1 || !keyPattern.MatchString(key[0]) {
		writeProblem(w, problem(400, "Invalid idempotency key", "Supply one Idempotency-Key of 16 to 128 letters, digits, hyphens or underscores."))
		return
	}
	var body SourceConsentDecision
	if !decodeJSON(w, r, &body) {
		return
	}
	receipt, err := a.Store.AppendSourceConsent(r.Context(), source, key[0], body)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if receipt.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, receipt)
}

// serveConsentAuthorities lists, approves or revokes consent authorities. Reading needs profile_reader;
// approving and revoking decide which sources may report consent and need privacy_admin.
func (a *API) serveConsentAuthorities(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.URL.Path == operatorConsentAuthorityPath {
		if !allow(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		if r.Method == http.MethodGet {
			if !a.authorizeConsent(w, r, identity.RoleProfileReader) {
				return
			}
			items, err := a.Store.ConsentAuthorities(r.Context())
			if err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"items": items})
			return
		}
		operator, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin)
		if !ok {
			return
		}
		var body ConsentAuthorityRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		actor := operator.Subject
		if actor == "" {
			actor = "development-bearer"
		}
		result, err := a.Store.ApproveConsentAuthority(r.Context(), body, actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, result)
		return
	}
	id, revoke := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorConsentAuthorityPath+"/"), "/revoke")
	if !revoke || !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if !allow(w, r, http.MethodPost) {
		return
	}
	operator, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin)
	if !ok {
		return
	}
	actor := operator.Subject
	if actor == "" {
		actor = "development-bearer"
	}
	result, err := a.Store.RevokeConsentAuthority(r.Context(), id, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
