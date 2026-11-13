package platform

import (
	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorAttributesPath = "/v1/operator/attributes"

type accessPolicyRequest struct {
	Readers []profiles.Reader `json:"readers"`
	Sources []string          `json:"sources"`
}

type attributeRequest struct {
	Type      profiles.AttributeType `json:"type"`
	MaxLength *int32                 `json:"max_length"`
	Values    []string               `json:"values"`
}

// serveAttributes reads and revises typed attribute definitions. Revising needs the
// governance_admin role.
func (a *API) serveAttributes(w http.ResponseWriter, r *http.Request) {
	path, access := "", false
	if r.URL.Path != operatorAttributesPath {
		path, access = strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorAttributesPath+"/"), "/access")
		if !profiles.Name.MatchString(path) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	methods := []string{http.MethodGet}
	if path != "" {
		methods = append(methods, http.MethodPost)
	}
	if !allow(w, r, methods...) || !a.authorizeConsent(w, r, writeRole(r, identity.RoleGovernanceAdmin)) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if access {
		a.serveAccessPolicy(w, r, path)
		return
	}
	if r.Method == http.MethodGet {
		var items []profiles.AttributeDefinition
		var err error
		if path == "" {
			items, err = a.Store.AttributeDefinitions(r.Context())
		} else {
			items, err = a.Store.AttributeRevisions(r.Context(), path)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	// Without If-Match the request defines a new attribute; with it, it revises the quoted revision.
	expected, ok := expectedRevision(w, r)
	if !ok {
		return
	}
	var body attributeRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	d, err := a.Store.DefineAttribute(r.Context(), profiles.AttributeDefinition{Path: path, Type: body.Type, MaxLength: body.MaxLength, Values: body.Values}, expected)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(int(d.Revision))+`"`)
	writeJSON(w, 201, d)
}

// serveAccessPolicy reads or replaces who may read a defined attribute. The caller has authorized
// the operator; replacing needs the governance_admin role.
func (a *API) serveAccessPolicy(w http.ResponseWriter, r *http.Request, path string) {
	if r.Method == http.MethodGet {
		items, err := a.Store.AccessPolicyRevisions(r.Context(), path)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	expected, ok := expectedRevision(w, r)
	if !ok {
		return
	}
	var body accessPolicyRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	p, err := a.Store.SetAccessPolicy(r.Context(), profiles.AccessPolicy{Path: path, Readers: body.Readers, Sources: body.Sources}, expected)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(int(p.Revision))+`"`)
	writeJSON(w, 201, p)
}

// expectedRevision reads an optional quoted revision from If-Match; absent means none exists yet.
func expectedRevision(w http.ResponseWriter, r *http.Request) (int32, bool) {
	etag := r.Header.Get("If-Match")
	if etag == "" {
		return 0, true
	}
	expected, err := strconv.ParseInt(strings.Trim(etag, `"`), 10, 32)
	if !versionPattern.MatchString(etag) || err != nil {
		writeProblem(w, problem(400, "Invalid precondition", "Use a quoted positive revision."))
		return 0, false
	}
	return int32(expected), true
}
