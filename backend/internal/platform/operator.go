package platform

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorProfilesPath = "/v1/operator/profiles"

func (a *API) serveOperator(w http.ResponseWriter, r *http.Request) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	provided := sha256.Sum256([]byte(token))
	expected := sha256.Sum256([]byte(a.Config.OperatorToken))
	if !bearer || a.Config.OperatorToken == "" || subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A configured development operator credential is required."))
		return
	}
	collection := r.URL.Path == operatorProfilesPath
	var id string
	if !collection {
		var detail bool
		id, detail = strings.CutPrefix(r.URL.Path, operatorProfilesPath+"/")
		if !detail || !profiles.ID.MatchString(id) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	a.serveProfileRead(w, r, id, collection)
}

func (a *API) serveProfileRead(w http.ResponseWriter, r *http.Request, id string, collection bool) {
	ctx := r.Context()
	if !collection {
		p, err := a.Store.Get(ctx, id)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, p.Version))
		writeJSON(w, 200, p)
		return
	}
	q := r.URL.Query()
	for k, values := range q {
		if (k != "cursor" && k != "limit") || len(values) != 1 {
			writeProblem(w, problem(400, "Invalid query", "Only cursor and limit are supported."))
			return
		}
	}
	cursor := q.Get("cursor")
	limit := 50
	var err error
	if q.Get("limit") != "" {
		limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 100 || (cursor != "" && !profiles.ID.MatchString(cursor)) {
		writeProblem(w, problem(400, "Invalid query", "Use a UUID cursor and limit between 1 and 100."))
		return
	}
	p, err := a.Store.List(ctx, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, p)
}
