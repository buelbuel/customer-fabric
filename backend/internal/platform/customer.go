package platform

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

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
	session, ok := a.authenticateCustomer(w, r)
	if !ok {
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
	q := db.New(a.Store.Pool)
	var id string
	if r.Method == http.MethodPut {
		id, err = a.Store.ResolveCustomerProfile(ctx, session.Subject)
	} else {
		id, err = q.GetCustomerProfileID(ctx, session.Subject)
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
