package platform

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorAttributesPath = "/v1/operator/attributes"

type attributeRequest struct {
	Type      profiles.AttributeType `json:"type"`
	MaxLength *int32                 `json:"max_length"`
	Values    []string               `json:"values"`
}

// serveAttributes reads and revises typed attribute definitions. Revising needs the development
// operator bearer, because no operator write role exists yet.
func (a *API) serveAttributes(w http.ResponseWriter, r *http.Request) {
	path := ""
	if r.URL.Path != operatorAttributesPath {
		path = strings.TrimPrefix(r.URL.Path, operatorAttributesPath+"/")
		if !profiles.Name.MatchString(path) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	methods := []string{http.MethodGet}
	if path != "" {
		methods = append(methods, http.MethodPost)
	}
	if !allow(w, r, methods...) || !a.authorizeConsent(w, r, r.Method == http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
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
	expected := int64(0)
	if etag := r.Header.Get("If-Match"); etag != "" {
		var err error
		if !versionPattern.MatchString(etag) {
			err = strconv.ErrSyntax
		} else {
			expected, err = strconv.ParseInt(etag[1:len(etag)-1], 10, 32)
		}
		if err != nil {
			writeProblem(w, problem(400, "Invalid precondition", "Use a quoted positive revision."))
			return
		}
	}
	var body attributeRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	d, err := a.Store.DefineAttribute(r.Context(), profiles.AttributeDefinition{Path: path, Type: body.Type, MaxLength: body.MaxLength, Values: body.Values}, int32(expected))
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(int(d.Revision))+`"`)
	writeJSON(w, 201, d)
}
