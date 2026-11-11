package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorSubscriptionsPath = "/v1/operator/notification-subscriptions"

// serveSubscriptions lists, creates, suspends or resumes notification subscriptions, and lists one's
// recorded deliveries. Reading needs profile_reader; changing a subscription authorizes external
// processing and needs privacy_admin.
func (a *API) serveSubscriptions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == operatorSubscriptionsPath {
		if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		if r.URL.RawQuery != "" {
			writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
			return
		}
		items, err := a.Store.Subscriptions(r.Context())
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	name := strings.TrimPrefix(r.URL.Path, operatorSubscriptionsPath+"/")
	name, state := strings.CutSuffix(name, "/state")
	deliveries := false
	if !state {
		name, deliveries = strings.CutSuffix(name, "/deliveries")
	}
	if !profiles.Name.MatchString(name) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if deliveries {
		if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		items, err := a.Store.SubscriptionDeliveries(r.Context(), name)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	if !allow(w, r, http.MethodPost) {
		return
	}
	operator, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin)
	if !ok {
		return
	}
	if state {
		var body struct {
			State string `json:"state"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		result, err := a.Store.SetSubscriptionState(r.Context(), name, body.State)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	var body SubscriptionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	actor := operator.Subject
	if actor == "" {
		actor = "development-bearer"
	}
	result, err := a.Store.CreateSubscription(r.Context(), name, body, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}
