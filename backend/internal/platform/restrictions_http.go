package platform

import (
	"net/http"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
)

const operatorRestrictionsDuePath = "/v1/operator/restrictions/due"

// serveRestriction reads a profile's restriction, or restricts or lifts it. Reading needs profile_reader
// (already checked by serveOperator); deciding needs privacy_admin.
func (a *API) serveRestriction(w http.ResponseWriter, r *http.Request, profileID string) {
	if !allow(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.Method == http.MethodGet {
		result, err := a.Store.Restriction(r.Context(), profileID)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	operator, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin)
	if !ok {
		return
	}
	var body struct {
		Action   string `json:"action"`
		Reason   string `json:"reason"`
		ReviewBy string `json:"review_by"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	actor := operator.Subject
	if actor == "" {
		actor = "development-bearer"
	}
	result, err := a.Store.DecideRestriction(r.Context(), profileID, body.Action, body.Reason, body.ReviewBy, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// serveRestrictionsDue lists restrictions whose review date has passed. They stay in force until lifted.
func (a *API) serveRestrictionsDue(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) {
		return
	}
	if _, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin); !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	items, err := a.Store.RestrictionsDue(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
