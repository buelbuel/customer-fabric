package platform

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorProfilesPath = "/v1/operator/profiles"

func (a *API) serveOperator(w http.ResponseWriter, r *http.Request) {
	subject, ok := a.authorizeOperatorRead(w, r)
	if !ok {
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
	if subject != "" {
		resource := "profile_detail"
		if collection {
			resource = "profile_list"
		}
		slog.InfoContext(r.Context(), "operator read authorized", "operator_subject", subject, "action", resource)
	}
	a.serveProfileRead(w, r, id, collection)
}

// authorizeOperatorRead writes the refusal itself, so callers only stop on false.
func (a *API) authorizeOperatorRead(w http.ResponseWriter, r *http.Request) (string, bool) {
	subject, err := a.authorizeOperator(r)
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrOperatorPermission):
			writeProblem(w, problem(403, "Forbidden", "Operator MFA and profile read permission are required."))
		case errors.Is(err, identity.ErrUnavailable):
			writeProblem(w, problem(503, "Identity service unavailable", "Operator authentication is temporarily unavailable."))
		default:
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		}
		return "", false
	}
	return subject, true
}

func (a *API) authorizeOperator(r *http.Request) (string, error) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if a.operatorKratos != nil {
		var cookies []*http.Cookie
		origin := a.Config.OperatorBrowserOrigin
		if origin != "" && len(r.Header.Values("Origin")) == 1 && r.Header.Get("Origin") == origin {
			cookies = r.CookiesNamed("cf_operator_" + strings.TrimPrefix(origin, "http://127.0.0.1:"))
		}
		if len(cookies) > 1 || (r.Header.Get("Authorization") != "" && (!bearer || token == "" || len(cookies) != 0)) {
			return "", identity.ErrInvalidSession
		}
		var cookie *http.Cookie
		if len(cookies) == 1 {
			cookie = cookies[0]
		}
		return a.operatorKratos.OperatorSubject(r.Context(), token, cookie)
	}
	provided := sha256.Sum256([]byte(token))
	expected := sha256.Sum256([]byte(a.Config.OperatorToken))
	if !bearer || a.Config.OperatorToken == "" || subtle.ConstantTimeCompare(provided[:], expected[:]) != 1 {
		return "", identity.ErrInvalidSession
	}
	return "", nil
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
