package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	operatorDestinationsPath = "/v1/operator/destinations"
	operatorActivationsPath  = "/v1/operator/activations/"
)

// serveDestinations lists and registers test webhooks. Registering needs the development operator bearer.
func (a *API) serveDestinations(w http.ResponseWriter, r *http.Request) {
	name := ""
	if r.URL.Path != operatorDestinationsPath {
		name = strings.TrimPrefix(r.URL.Path, operatorDestinationsPath+"/")
		if !profiles.Name.MatchString(name) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	method := http.MethodGet
	if name != "" {
		method = http.MethodPost
	}
	if !allow(w, r, method) || !a.authorizeConsent(w, r, method == http.MethodPost) {
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
	var body struct {
		URL string `json:"url"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	d, err := a.Store.CreateDestination(r.Context(), name, body.URL, a.Config.WebhookHosts)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, d)
}

// serveActivate queues a segment's current members for a destination.
func (a *API) serveActivate(w http.ResponseWriter, r *http.Request, segment string) {
	if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, true) {
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
		if !allow(w, r, http.MethodPost) || !a.authorizeConsent(w, r, true) {
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
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, false) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	result, err := a.Store.Activation(r.Context(), id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// serveActivations lists recent activations with their delivery counts.
func (a *API) serveActivations(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, false) {
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
