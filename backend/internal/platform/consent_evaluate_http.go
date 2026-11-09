package platform

import (
	"net/http"
	"strings"
)

const evaluatePath = "/v1/operator/consent/effective/"

// serveEffectivePermission answers "is this customer's data permitted for this purpose?" deterministically
// from committed evidence. It never mutates. Missing, ambiguous or no-policy evidence fails closed.
func (a *API) serveEffectivePermission(w http.ResponseWriter, r *http.Request) {
	if _, err := a.authorizeOperator(r); err != nil {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, evaluatePath)
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || !validID(parts[0]) || !validID(parts[1]) {
		writeProblem(w, problem(404, "Not found", "Expected /v1/operator/consent/effective/{profileId}/{purposeId}."))
		return
	}
	ev, err := a.Store.EvaluateCustomerPermission(r.Context(), parts[0], parts[1])
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, ev)
}
