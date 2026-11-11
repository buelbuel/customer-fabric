package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorAudienceJobsPath = "/v1/operator/audience-jobs"

// serveSubmitAudienceJob queues a snapshot or activation job for a segment. It answers 202 for a newly
// queued job and 200 when the Idempotency-Key already named this job.
func (a *API) serveSubmitAudienceJob(w http.ResponseWriter, r *http.Request, segment, kind string) {
	role := identity.RoleAudienceManager
	if kind == JobActivation {
		role = identity.RoleActivationOperator
	}
	if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, role) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !keyPattern.MatchString(key) {
		writeProblem(w, problem(400, "Invalid idempotency key", "Supply one Idempotency-Key of 16 to 128 letters, digits, hyphens or underscores."))
		return
	}
	destination := ""
	if kind == JobActivation {
		var body struct {
			Destination string `json:"destination"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if !profiles.Name.MatchString(body.Destination) {
			writeProblem(w, problem(422, "Invalid job", "destination must be a registered destination."))
			return
		}
		destination = body.Destination
	}
	job, created, err := a.Store.SubmitAudienceJob(r.Context(), kind, segment, destination, key)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusAccepted
	}
	writeJSON(w, status, job)
}

// serveAudienceJobs lists recent jobs, reads one, or cancels one. Status carries counts and reason
// codes only, never customers; a completed job names the snapshot or activation to read them from.
func (a *API) serveAudienceJobs(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == operatorAudienceJobsPath {
		if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		if r.URL.RawQuery != "" {
			writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
			return
		}
		items, err := a.Store.AudienceJobs(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	id, cancel := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorAudienceJobsPath+"/"), "/cancel")
	if !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	method, role := http.MethodGet, identity.RoleProfileReader
	if cancel {
		// Cancelling needs the role that may submit the job's kind.
		method, role = http.MethodPost, identity.RoleAudienceManager
		if r.Method == http.MethodPost {
			if job, err := a.Store.AudienceJob(r.Context(), id); err == nil && job.Kind == JobActivation {
				role = identity.RoleActivationOperator
			}
		}
	}
	if !allow(w, r, method) || !a.authorizeConsent(w, r, role) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	var job AudienceJob
	var err error
	if cancel {
		job, err = a.Store.CancelAudienceJob(r.Context(), id)
	} else {
		job, err = a.Store.AudienceJob(r.Context(), id)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, job)
}

// serveSnapshotMembers pages a snapshot's members, as anchor IDs.
func (a *API) serveSnapshotMembers(w http.ResponseWriter, r *http.Request, id string) {
	if !allow(w, r, http.MethodGet) || !a.authorizeMembershipRead(w, r, "segment_snapshot_members") {
		return
	}
	page, err := pageQuery(r.URL.Query(), []string{"cursor"}, 500, 1000)
	if err != nil {
		writeProblem(w, problem(400, "Invalid query", "Use an optional UUID cursor and a limit between 1 and 1000."))
		return
	}
	result, err := a.Store.SnapshotMembers(r.Context(), id, page.Cursors["cursor"], page.Limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}
