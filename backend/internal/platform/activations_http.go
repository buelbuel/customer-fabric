package platform

import (
	"net/http"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	operatorDestinationsPath = "/v1/operator/destinations"
	operatorSecretsPath      = "/v1/operator/destination-secrets"
	operatorActivationsPath  = "/v1/operator/activations/"
)

// serveDestinations lists and registers webhooks and rotates their signing secrets.
func (a *API) serveDestinations(w http.ResponseWriter, r *http.Request) {
	name, rotate := "", false
	if r.URL.Path != operatorDestinationsPath {
		name = strings.TrimPrefix(r.URL.Path, operatorDestinationsPath+"/")
		name, rotate = strings.CutSuffix(name, "/signing-secret")
		if !profiles.Name.MatchString(name) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	method := http.MethodGet
	if name != "" {
		method = http.MethodPost
	}
	if !allow(w, r, method) || !a.authorizeConsent(w, r, writeRole(r, identity.RoleActivationOperator)) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if name == "" {
		items, err := a.Store.Destinations(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if rotate {
		var body struct {
			OverlapSeconds *int64 `json:"overlap_seconds"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		if body.OverlapSeconds == nil || *body.OverlapSeconds < 0 || *body.OverlapSeconds > int64(MaxSecretOverlap/time.Second) {
			writeProblem(w, problem(422, "Invalid rotation", "overlap_seconds must be between 0 and 604800."))
			return
		}
		rotation, err := a.Store.RotateDestinationSecret(r.Context(), name, time.Duration(*body.OverlapSeconds)*time.Second)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, rotation)
		return
	}
	var body struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	d, err := a.Store.CreateDestination(r.Context(), name, body.URL, a.Config.Egress())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, d)
}

// serveSecretStatus reports which destinations can sign now and what awaits an operator, without any
// secret. It is the health status for webhook signing keys.
func (a *API) serveSecretStatus(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleActivationOperator) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	status, err := a.Store.SecretStatus(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, status)
}

// serveActivate queues a segment's current members for a destination.
func (a *API) serveActivate(w http.ResponseWriter, r *http.Request, segment string) {
	if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, identity.RoleActivationOperator) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	var body struct {
		Destination string `json:"destination"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !profiles.Name.MatchString(body.Destination) {
		writeProblem(w, problem(422, "Invalid activation", "destination must be a registered destination."))
		return
	}
	result, err := a.Store.Activate(r.Context(), segment, body.Destination)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}

func (a *API) serveActivation(w http.ResponseWriter, r *http.Request) {
	id, redrive := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, operatorActivationsPath), "/redrive")
	if !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if redrive {
		if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, identity.RoleActivationOperator) {
			return
		}
		requeued, err := a.Store.RedriveFailed(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]int64{"requeued": requeued})
		return
	}
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	page, err := pageQuery(r.URL.Query(), []string{"cursor"}, 100, 1000)
	if err != nil {
		writeProblem(w, problem(400, "Invalid query", "Use an optional UUID cursor and a limit between 1 and 1000."))
		return
	}
	result, err := a.Store.Activation(r.Context(), id, page.Cursors["cursor"], page.Limit)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// serveActivations lists recent activations with their delivery counts.
func (a *API) serveActivations(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	items, err := a.Store.RecentActivations(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

const operatorAudienceMetricsPath = "/v1/operator/audience-metrics"

func (a *API) serveAudienceMetrics(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	metrics, err := a.Store.AudienceMetrics(r.Context())
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, metrics)
}
