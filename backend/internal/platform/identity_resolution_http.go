package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const resolutionKeysPath = "/v1/operator/identity-resolution-keys"

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
	actor := operator.Subject
	if actor == "" {
		actor = "development-bearer"
	}
	key, err := a.Store.SetResolutionKey(r.Context(), namespace, body.Sources, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, key)
}

// serveIdentityResolutions lists the resolution decisions that put records on a profile.
func (a *API) serveIdentityResolutions(w http.ResponseWriter, r *http.Request, profileID string) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	items, err := a.Store.IdentityResolutions(r.Context(), profileID)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
