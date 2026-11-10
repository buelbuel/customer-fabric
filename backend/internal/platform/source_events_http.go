package platform

import (
	"bytes"
	"encoding/json"
	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	sourceEventsPath    = "/v1/source-events"
	operatorSourcesPath = "/v1/operator/sources/"
)

// serveSourceEvents accepts one event for the source the credential maps to. Nothing in the body
// can name a source, profile or customer.
func (a *API) serveSourceEvents(w http.ResponseWriter, r *http.Request, source string) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeProblem(w, problem(413, "Payload too large", "Request body must not exceed 64 KiB."))
		return
	}
	event, ok := decodeSourceEvent(w, raw)
	if !ok {
		return
	}
	if err = event.Validate(); err != nil {
		writeProblem(w, problem(422, "Invalid event", err.Error()))
		return
	}
	receipt, err := a.Store.IngestSourceEvent(r.Context(), source, event)
	if err != nil {
		fail(w, err)
		return
	}
	status := 200
	if receipt.Created {
		status = 201
	}
	writeJSON(w, status, receipt)
}

// decodeSourceEvent matches field names exactly. encoding/json alone would accept case aliases and
// let a duplicate key overwrite an earlier one.
func decodeSourceEvent(w http.ResponseWriter, raw []byte) (ingest.Event, bool) {
	var event ingest.Event
	invalid := func(detail string) (ingest.Event, bool) {
		writeProblem(w, problem(400, "Invalid event", detail))
		return event, false
	}
	var fields map[string]json.RawMessage
	if !utf8.Valid(raw) || !json.Valid(raw) || duplicateKeys(raw) || json.Unmarshal(raw, &fields) != nil || fields == nil {
		return invalid("Supply one JSON object without duplicate keys.")
	}
	for name := range fields {
		switch name {
		case "event_id", "type", "record", "source_epoch", "sequence", "occurred_at", "set", "remove":
		default:
			return invalid("Unknown event field.")
		}
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(fields["record"], &record) != nil {
		return invalid("record must be an object.")
	}
	for name := range record {
		if name != "namespace" && name != "value" {
			return invalid("record fields must be namespace and value.")
		}
	}
	for _, name := range []string{"set", "remove"} {
		if value, present := fields[name]; present && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			writeProblem(w, problem(422, "Invalid event", "Use remove to delete an attribute; event fields cannot be null."))
			return event, false
		}
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if dec.Decode(&event) != nil {
		return invalid("Field types do not match the event schema; occurred_at must be RFC 3339.")
	}
	return event, true
}

// serveSourceEpoch reads or advances a source's epoch. Advancing needs the development operator
// bearer, because no operator write role exists yet.
func (a *API) serveSourceEpoch(w http.ResponseWriter, r *http.Request) {
	name, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorSourcesPath), "/epoch")
	if !ok || !profiles.Name.MatchString(name) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if !allow(w, r, http.MethodGet, http.MethodPost) || !a.authorizeConsent(w, r, writeRole(r, identity.RoleSourceAdmin)) || !emptyLinkingBody(w, r) {
		return
	}
	var epoch int64
	var err error
	if r.Method == http.MethodGet {
		epoch, err = a.Store.SourceEpoch(r.Context(), name)
	} else {
		etag := r.Header.Get("If-Match")
		if etag == "" {
			writeProblem(w, problem(428, "Precondition required", "Supply the epoch you read in If-Match."))
			return
		}
		var expected int64
		if versionPattern.MatchString(etag) {
			expected, err = strconv.ParseInt(etag[1:len(etag)-1], 10, 64)
		}
		if !versionPattern.MatchString(etag) || err != nil {
			writeProblem(w, problem(400, "Invalid precondition", "Use a quoted positive epoch."))
			return
		}
		epoch, err = a.Store.AdvanceSourceEpoch(r.Context(), name, expected)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"source": name, "epoch": epoch})
}
