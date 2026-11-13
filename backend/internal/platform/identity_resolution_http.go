package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	resolutionKeysPath     = "/v1/operator/identity-resolution-keys"
	resolutionPoliciesPath = "/v1/operator/attribute-resolution-policies"
	profileLinksPath       = "/v1/operator/profile-links/"
)

// serveResolutionKeys lists the trusted identity keys (profile_reader) or declares, replaces and removes
// one (governance_admin).
func (a *API) serveResolutionKeys(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	namespace := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, resolutionKeysPath), "/")
	if namespace == "" {
		if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		items, err := a.Store.ResolutionKeys(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if !profiles.Name.MatchString(namespace) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if !allow(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	operator, ok := a.authorizeRole(w, r, identity.RoleGovernanceAdmin)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		if err := a.Store.DeleteResolutionKey(r.Context(), namespace); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Sources []string `json:"sources"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	key, err := a.Store.SetResolutionKey(r.Context(), namespace, body.Sources, operator.Subject)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, key)
}

// serveResolutionPolicies lists the attribute resolution policies (profile_reader) or sets and removes
// one (governance_admin).
func (a *API) serveResolutionPolicies(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	path := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, resolutionPoliciesPath), "/")
	if path == "" {
		if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		items, err := a.Store.ResolutionPolicies(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if !profiles.Name.MatchString(path) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if !allow(w, r, http.MethodPut, http.MethodDelete) {
		return
	}
	operator, ok := a.authorizeRole(w, r, identity.RoleGovernanceAdmin)
	if !ok {
		return
	}
	if r.Method == http.MethodDelete {
		if err := a.Store.DeleteResolutionPolicy(r.Context(), path); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	var body struct {
		Strategy       string   `json:"strategy"`
		SourcePriority []string `json:"source_priority"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	policy, err := a.Store.SetResolutionPolicy(r.Context(), path, body.Strategy, body.SourcePriority, operator.Subject)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, policy)
}

// serveLinkRemoval is an identity reviewer's correction of a wrong identity link.
func (a *API) serveLinkRemoval(w http.ResponseWriter, r *http.Request) {
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, profileLinksPath), "/remove")
	if !ok || !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if !allow(w, r, http.MethodPost) {
		return
	}
	operator, ok := a.authorizeRole(w, r, identity.RoleIdentityReviewer)
	if !ok {
		return
	}
	var body struct {
		Reference string `json:"reference"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	link, err := a.Store.RemoveLink(r.Context(), id, operator.Subject, body.Reference)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, link)
}

// serveIdentityHistory lists the identity links and decisions of the unified customer a profile belongs
// to.
func (a *API) serveIdentityHistory(w http.ResponseWriter, r *http.Request, profileID string) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	links, decisions, err := a.Store.IdentityHistory(r.Context(), profileID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"links": links, "decisions": decisions})
}
