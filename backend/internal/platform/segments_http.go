package platform

import (
	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/customer-fabric/customer-fabric/backend/internal/segments"
)

const operatorSegmentsPath = "/v1/operator/segments"

// serveSegments reads and revises segment definitions and previews their membership. Revising needs
// the development operator bearer, because no operator write role exists yet.
func (a *API) serveSegments(w http.ResponseWriter, r *http.Request) {
	name, preview, activate, snapshots := "", false, false, false
	if r.URL.Path != operatorSegmentsPath {
		name = strings.TrimPrefix(r.URL.Path, operatorSegmentsPath+"/")
		name, preview = strings.CutSuffix(name, "/preview")
		if !preview {
			name, activate = strings.CutSuffix(name, "/activations")
		}
		if !preview && !activate {
			name, snapshots = strings.CutSuffix(name, "/snapshots")
		}
		if !profiles.Name.MatchString(name) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	if preview {
		a.serveSegmentPreview(w, r, name)
		return
	}
	if activate {
		a.serveActivate(w, r, name)
		return
	}
	if snapshots {
		a.serveSnapshots(w, r, name)
		return
	}
	methods := []string{http.MethodGet}
	if name != "" {
		methods = append(methods, http.MethodPost)
	}
	if !allow(w, r, methods...) || !a.authorizeConsent(w, r, writeRole(r, identity.RoleAudienceManager)) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.Method == http.MethodGet {
		var items []segments.Segment
		var err error
		if name == "" {
			items, err = a.Store.Segments(r.Context())
		} else {
			items, err = a.Store.SegmentRevisions(r.Context(), name)
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	// Without If-Match the request defines a new segment; with it, it revises the quoted revision.
	expected, ok := expectedRevision(w, r)
	if !ok {
		return
	}
	var rule segments.Rule
	if !decodeJSON(w, r, &rule) {
		return
	}
	segment, err := a.Store.DefineSegment(r.Context(), name, rule, expected)
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("ETag", `"`+strconv.Itoa(int(segment.Revision))+`"`)
	writeJSON(w, 201, segment)
}

// serveSegmentPreview evaluates a segment for an operator with profile read permission. It discloses
// membership and provenance, never attribute values.
func (a *API) serveSegmentPreview(w http.ResponseWriter, r *http.Request, name string) {
	subject, ok := a.authorizeOperatorRead(w, r)
	if !ok || !allow(w, r, http.MethodGet) {
		return
	}
	q := r.URL.Query()
	for k, values := range q {
		if (k != "cursor" && k != "limit") || len(values) != 1 {
			writeProblem(w, problem(400, "Invalid query", "Only cursor and limit are supported."))
			return
		}
	}
	cursor, limit := q.Get("cursor"), 20
	var err error
	if q.Get("limit") != "" {
		limit, err = strconv.Atoi(q.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 100 || (cursor != "" && !profiles.ID.MatchString(cursor)) {
		writeProblem(w, problem(400, "Invalid query", "Use a UUID cursor and limit between 1 and 100."))
		return
	}
	if subject != "" {
		slog.InfoContext(r.Context(), "operator read authorized", "operator_subject", subject, "action", "segment_preview")
	}
	result, err := a.Store.PreviewSegment(r.Context(), name, cursor, limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

const operatorSnapshotsPath = "/v1/operator/segment-snapshots/"

// serveSnapshots lists a segment's snapshots or takes one. Taking one needs the development operator bearer.
func (a *API) serveSnapshots(w http.ResponseWriter, r *http.Request, segment string) {
	if !allow(w, r, http.MethodGet, http.MethodPost) {
		return
	}
	if r.Method == http.MethodPost && !a.authorizeConsent(w, r, identity.RoleAudienceManager) {
		return
	}
	if r.Method == http.MethodGet && !a.authorizeMembershipRead(w, r, "segment_snapshot_list") {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.Method == http.MethodGet {
		items, err := a.Store.Snapshots(r.Context(), segment)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	snapshot, err := a.Store.TakeSnapshot(r.Context(), segment)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, snapshot)
}

// serveSnapshotChanges compares a snapshot with an earlier one of the same segment.
func (a *API) serveSnapshotChanges(w http.ResponseWriter, r *http.Request) {
	id, changes := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorSnapshotsPath), "/changes")
	if !changes || !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if !allow(w, r, http.MethodGet) || !a.authorizeMembershipRead(w, r, "segment_snapshot_changes") {
		return
	}
	q := r.URL.Query()
	since := q.Get("since")
	if len(q) != 1 || len(q["since"]) != 1 || !profiles.ID.MatchString(since) {
		writeProblem(w, problem(400, "Invalid query", "Supply exactly one since snapshot ID."))
		return
	}
	result, err := a.Store.CompareSnapshots(r.Context(), id, since)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// authorizeMembershipRead authorizes a read that discloses which customers belong to a segment and records
// the operator who made it, as previews do.
func (a *API) authorizeMembershipRead(w http.ResponseWriter, r *http.Request, action string) bool {
	subject, ok := a.authorizeOperatorRead(w, r)
	if ok && subject != "" {
		slog.InfoContext(r.Context(), "operator read authorized", "operator_subject", subject, "action", action)
	}
	return ok
}
