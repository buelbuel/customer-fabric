package platform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

const customerProfilePath = "/v1/customer/profile"

func (a *API) serveCustomer(w http.ResponseWriter, r *http.Request) {
	if a.kratos == nil || r.URL.Path != customerProfilePath {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		w.Header().Set("Allow", "GET, PUT")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	tokens := r.Header.Values("X-Session-Token")
	cookies := r.CookiesNamed("cf_customer_" + strings.TrimPrefix(a.Config.CustomerBrowserOrigin, "http://127.0.0.1:"))
	browser := a.Config.CustomerBrowserOrigin != "" && len(tokens) == 0 && len(cookies) == 1
	if len(r.Header.Values("Authorization")) != 0 || (!browser && (len(tokens) != 1 || len(cookies) != 0)) {
		writeProblem(w, problem(401, "Unauthorized", "A customer session is required."))
		return
	}
	if browser && r.Method == http.MethodPut && (len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != a.Config.CustomerBrowserOrigin) {
		writeProblem(w, problem(403, "Origin rejected", "Browser provisioning requires the configured origin."))
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "The session determines the customer profile."))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err != nil || len(body) != 0 {
		writeProblem(w, problem(400, "Invalid payload", "This resource does not accept a request body."))
		return
	}
	ctx := r.Context()
	var subject string
	if browser {
		subject, err = a.kratos.VerifiedBrowserSubject(ctx, cookies[0])
	} else {
		subject, err = a.kratos.VerifiedSubject(ctx, tokens[0])
	}
	switch {
	case errors.Is(err, identity.ErrInvalidSession):
		writeProblem(w, problem(401, "Unauthorized", "A valid customer session is required."))
		return
	case errors.Is(err, identity.ErrUnverifiedAddress):
		writeProblem(w, problem(403, "Verification required", "Verify the account email before accessing a profile."))
		return
	case err != nil:
		writeProblem(w, problem(503, "Identity service unavailable", "Customer session verification could not be completed."))
		return
	}

	q := db.New(a.Store.Pool)
	var id string
	if r.Method == http.MethodPut {
		id, err = a.Store.ResolveCustomerProfile(ctx, subject)
	} else {
		id, err = q.GetCustomerProfileID(ctx, subject)
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, problem(404, "Not found", "The customer profile has not been provisioned."))
			return
		}
	}
	if err != nil {
		fail(w, err)
		return
	}
	row, err := q.GetProfile(ctx, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, struct {
		ID         string          `json:"id"`
		Version    int64           `json:"version"`
		Attributes json.RawMessage `json:"attributes"`
	}{ID: row.ID, Version: row.Version, Attributes: row.Attributes})
}
