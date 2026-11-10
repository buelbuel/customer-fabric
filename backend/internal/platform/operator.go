package platform

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

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
	candidates := false
	if !collection {
		var detail bool
		id, detail = strings.CutPrefix(r.URL.Path, operatorProfilesPath+"/")
		var reviews bool
		id, candidates = strings.CutSuffix(id, "/match-candidates")
		if !candidates {
			id, reviews = strings.CutSuffix(id, "/match-reviews")
		}
		if !detail || !profiles.ID.MatchString(id) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
		if reviews {
			a.serveMatchReviews(w, r, id, subject)
			return
		}
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	if candidates {
		if r.URL.RawQuery != "" {
			writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
			return
		}
		if subject != "" {
			slog.InfoContext(r.Context(), "operator read authorized", "operator_subject", subject, "action", "match_candidates")
		}
		result, err := a.Store.FindMatchCandidates(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, result)
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

// authorizeOperatorRead requires the profile_reader role. It writes the refusal itself, so callers only
// stop on false; the subject is empty in development bearer mode.
func (a *API) authorizeOperatorRead(w http.ResponseWriter, r *http.Request) (string, bool) {
	operator, ok := a.authorizeRole(w, r, identity.RoleProfileReader)
	return operator.Subject, ok
}

// authorizeRole authenticates an operator and requires one role. A named operator holds only the roles
// the operator issuer assigns; the development bearer holds every role and is never production
// authorization. The authorized actor is recorded for the write audit.
func (a *API) authorizeRole(w http.ResponseWriter, r *http.Request, role string) (identity.Operator, bool) {
	operator, err := a.authorizeOperator(r)
	if err == nil && !operator.Has(role) {
		writeProblem(w, problem(403, "Forbidden", fmt.Sprintf("This operation requires the %s role.", role)))
		return operator, false
	}
	if err != nil {
		switch {
		case errors.Is(err, identity.ErrOperatorPermission):
			writeProblem(w, problem(403, "Forbidden", "Operator MFA and an operator role are required."))
		case errors.Is(err, identity.ErrUnavailable):
			writeProblem(w, problem(503, "Identity service unavailable", "Operator authentication is temporarily unavailable."))
		default:
			w.Header().Set("WWW-Authenticate", "Bearer")
			writeProblem(w, problem(401, "Unauthorized", "A valid operator session is required."))
		}
		return operator, false
	}
	if audit, ok := r.Context().Value(auditKey{}).(*operatorAudit); ok {
		audit.actor = operator.Subject
		if audit.actor == "" {
			audit.actor = "development-bearer"
		}
	}
	return operator, true
}

func (a *API) authorizeOperator(r *http.Request) (identity.Operator, error) {
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if a.operatorKratos != nil {
		var cookies []*http.Cookie
		origin := a.Config.OperatorBrowserOrigin
		if origin != "" && len(r.Header.Values("Origin")) == 1 && r.Header.Get("Origin") == origin {
			cookies = r.CookiesNamed("cf_operator_" + strings.TrimPrefix(origin, "http://127.0.0.1:"))
		}
		if len(cookies) > 1 || (r.Header.Get("Authorization") != "" && (!bearer || token == "" || len(cookies) != 0)) {
			return identity.Operator{}, identity.ErrInvalidSession
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
		return identity.Operator{}, identity.ErrInvalidSession
	}
	return identity.Operator{Roles: identity.AllRoles()}, nil
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

// serveMatchReviews lists the review trail of a record or records a decision about one of its current
// candidates. Recording needs the development operator bearer, because no operator write role exists yet.
func (a *API) serveMatchReviews(w http.ResponseWriter, r *http.Request, id, subject string) {
	if !allow(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.Method == http.MethodGet {
		if subject != "" {
			slog.InfoContext(r.Context(), "operator read authorized", "operator_subject", subject, "action", "match_reviews")
		}
		items, err := a.Store.MatchReviews(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if !a.authorizeConsent(w, r, identity.RoleIdentityReviewer) {
		return
	}
	var body struct {
		CandidateProfileID string `json:"candidate_profile_id"`
		Decision           string `json:"decision"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !profiles.ID.MatchString(body.CandidateProfileID) {
		writeProblem(w, problem(422, "Invalid review", "candidate_profile_id must be a profile ID."))
		return
	}
	review, err := a.Store.ReviewMatch(r.Context(), id, body.CandidateProfileID, body.Decision)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, review)
}

// writeRole is role for a write and profile_reader for a read.
func writeRole(r *http.Request, role string) string {
	if r.Method == http.MethodGet {
		return identity.RoleProfileReader
	}
	return role
}

// operatorAudit carries the authorized actor of one operator write to the audit record.
type operatorAudit struct{ actor string }

type auditKey struct{}

// statusRecorder remembers the status a handler wrote.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(status int) {
	s.status = status
	s.ResponseWriter.WriteHeader(status)
}

// auditOperatorWrite runs an operator write and, if an operator was authorized and the write succeeded,
// records who changed which resource. The record is best effort: it follows the operation's own commit,
// so a failed insert is logged as an error rather than undoing the operation.
func (a *API) auditOperatorWrite(w http.ResponseWriter, r *http.Request, next func(http.ResponseWriter, *http.Request)) {
	audit := &operatorAudit{}
	recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
	next(recorder, r.WithContext(context.WithValue(r.Context(), auditKey{}, audit)))
	if audit.actor == "" || recorder.status < 200 || recorder.status > 299 {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer cancel()
	if err := a.Store.RecordOperatorAudit(ctx, audit.actor, r.Method, r.URL.Path, recorder.status); err != nil {
		slog.ErrorContext(r.Context(), "operator audit record failed", "method", r.Method, "resource", r.URL.Path)
	}
}

const operatorAuditPath = "/v1/operator/audit"

// serveOperatorAudit lists the most recent administrative writes for an audit_reader.
func (a *API) serveOperatorAudit(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleAuditReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	items, err := a.Store.OperatorAudit(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}
