package platform

import (
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const importsPath = "/v1/imports"

// serveImports lets a source import its own historical records. Nothing in a row can name another
// source, a profile or a customer; the credential decides the source.
func (a *API) serveImports(w http.ResponseWriter, r *http.Request, source string) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.URL.Path == importsPath {
		if !allow(w, r, http.MethodPost) {
			return
		}
		body := struct {
			MaxErrors *int32             `json:"max_errors"`
			Mapping   *ingest.CSVMapping `json:"csv"`
		}{}
		if !decodeJSON(w, r, &body) {
			return
		}
		maxErrors := int32(100)
		if body.MaxErrors != nil {
			maxErrors = *body.MaxErrors
		}
		if maxErrors < 0 || maxErrors > 100000 {
			writeProblem(w, problem(422, "Invalid import", "max_errors must be between 0 and 100000."))
			return
		}
		imp, err := a.Store.CreateImport(r.Context(), source, maxErrors, body.Mapping)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, imp)
		return
	}
	id, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, importsPath+"/"), "/")
	if !profiles.ID.MatchString(id) || (action != "" && action != "rows" && action != "complete") {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	method := http.MethodPost
	if action == "" {
		method = http.MethodGet
	}
	if !allow(w, r, method) {
		return
	}
	var imp Import
	var err error
	switch action {
	case "":
		imp, err = a.Store.GetImport(r.Context(), source, id)
	case "complete":
		imp, err = a.Store.CompleteImport(r.Context(), source, id)
	case "rows":
		media, _, mediaErr := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if mediaErr != nil || (media != "application/x-ndjson" && media != "text/csv") {
			writeProblem(w, problem(415, "Unsupported media type", "Use application/x-ndjson, or text/csv for a CSV import."))
			return
		}
		offset, parseErr := strconv.ParseInt(r.Header.Get("Import-Offset"), 10, 64)
		if parseErr != nil || offset < 1 {
			writeProblem(w, problem(400, "Invalid offset", "Import-Offset is the positive line number of the batch's first row."))
			return
		}
		body := http.MaxBytesReader(w, r.Body, 8<<20)
		if media == "text/csv" {
			imp, err = a.Store.ImportCSVBatch(r.Context(), source, id, offset, body)
		} else {
			imp, err = a.Store.ImportBatch(r.Context(), source, id, offset, body)
		}
		// A refused batch stops reading early; drain the bounded rest so the client receives the answer
		// instead of a reset connection.
		_, _ = io.Copy(io.Discard, body)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, imp)
}
