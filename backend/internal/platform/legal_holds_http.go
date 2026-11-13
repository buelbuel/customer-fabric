package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const legalHoldsPath = "/v1/operator/legal-holds"

// serveLegalHolds requests, approves, releases and reads legal holds (#30). Changes need privacy_admin;
// reading needs privacy_admin or audit_reader, and an audit reader never sees the legal reference.
func (a *API) serveLegalHolds(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, legalHoldsPath), "/")
	parts := strings.Split(rest, "/")
	switch {
	case rest == "" && r.Method == http.MethodPost:
		actor, ok := a.privacyAdmin(w, r)
		if !ok {
			return
		}
		var req HoldRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if !profiles.ID.MatchString(req.ProfileID) {
			writeProblem(w, problem(422, "Invalid legal hold", "profile_id must be a profile ID."))
			return
		}
		hold, err := a.Store.RequestHold(r.Context(), req, actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, hold)
	case rest == "":
		a.listLegalHolds(w, r)
	case len(parts) == 1 && profiles.ID.MatchString(parts[0]):
		redact, ok := a.holdReader(w, r)
		if !ok {
			return
		}
		if r.URL.RawQuery != "" {
			writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
			return
		}
		hold, err := a.Store.Hold(r.Context(), parts[0], redact)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, hold)
	case len(parts) == 2 && profiles.ID.MatchString(parts[0]) && parts[1] == "approve":
		actor, ok := a.privacyAdmin(w, r)
		if !ok {
			return
		}
		hold, err := a.Store.ApproveHold(r.Context(), parts[0], actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, hold)
	case len(parts) == 2 && profiles.ID.MatchString(parts[0]) && parts[1] == "release":
		actor, ok := a.privacyAdmin(w, r)
		if !ok {
			return
		}
		var body struct {
			Reason string `json:"reason"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		hold, err := a.Store.ReleaseHold(r.Context(), parts[0], body.Reason, actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, hold)
	default:
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
	}
}

// listLegalHolds lists the holds not released, a profile's holds (profile_id), or those due for review
// (due=true).
func (a *API) listLegalHolds(w http.ResponseWriter, r *http.Request) {
	redact, ok := a.holdReader(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	profileID, due := "", false
	for key, values := range q {
		switch {
		case len(values) != 1:
			writeProblem(w, problem(400, "Invalid query", "Repeat no query parameter."))
			return
		case key == "profile_id" && profiles.ID.MatchString(values[0]):
			profileID = values[0]
		case key == "due" && values[0] == "true":
			due = true
		default:
			writeProblem(w, problem(400, "Invalid query", "Use profile_id=<profile ID> or due=true."))
			return
		}
	}
	if due && profileID != "" {
		writeProblem(w, problem(400, "Invalid query", "Use profile_id=<profile ID> or due=true."))
		return
	}
	items, err := a.Store.Holds(r.Context(), profileID, due, redact)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

// holdReader authorizes reading holds and says whether the legal reference must be withheld: it is
// shown to privacy admins only.
func (a *API) holdReader(w http.ResponseWriter, r *http.Request) (redact bool, ok bool) {
	if !allow(w, r, http.MethodGet) {
		return false, false
	}
	operator, err := a.authorizeOperator(r)
	if err != nil {
		writeOperatorRefusal(w, err)
		return false, false
	}
	if !operator.Has(identity.RolePrivacyAdmin) && !operator.Has(identity.RoleAuditReader) {
		writeProblem(w, problem(403, "Forbidden", "This operation requires the privacy_admin or audit_reader role."))
		return false, false
	}
	return !operator.Has(identity.RolePrivacyAdmin), true
}
