package platform

import (
	"log/slog"
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorCustomersPath = "/v1/operator/customers/"

// serveCustomerView answers only operators. ADR 0007 keeps enterprise attributes, source identifiers
// and provenance out of customer-facing association reads, and source credentials cannot read profiles.
func (a *API) serveCustomerView(w http.ResponseWriter, r *http.Request) {
	if a.kratos == nil {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	subject, ok := a.authorizeOperatorRead(w, r)
	if !ok {
		return
	}
	id, view := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorCustomersPath), "/unified-view")
	if !view || !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if subject != "" {
		slog.InfoContext(r.Context(), "operator read authorized", "operator_subject", subject, "action", "customer_unified_view")
	}
	result, err := a.Store.CustomerView(r.Context(), id, profiles.ReaderOperator)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
