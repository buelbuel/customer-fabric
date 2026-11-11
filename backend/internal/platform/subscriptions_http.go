package platform

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const operatorSubscriptionsPath = "/v1/operator/notification-subscriptions"

// serveSubscriptions lists, creates, suspends, resumes and reapproves notification subscriptions, lists
// one's recorded deliveries and audit history, and requests and reads recoveries (ADR 0016). Reading
// needs profile_reader; changing what a subscription authorizes or sends needs privacy_admin.
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
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, operatorSubscriptionsPath+"/"), "/")
	name := parts[0]
	if !profiles.Name.MatchString(name) || len(parts) > 4 {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	switch {
	case len(parts) == 1:
		a.createSubscription(w, r, name)
	case len(parts) == 2 && parts[1] == "state":
		a.setSubscriptionState(w, r, name)
	case len(parts) == 2 && parts[1] == "deliveries":
		a.readSubscription(w, r, func() (any, error) { return a.Store.SubscriptionDeliveries(r.Context(), name) })
	case len(parts) == 2 && parts[1] == "audit":
		a.readSubscription(w, r, func() (any, error) { return a.Store.SubscriptionAudit(r.Context(), name) })
	case len(parts) == 2 && parts[1] == "reapprove":
		a.reapproveSubscription(w, r, name)
	case len(parts) == 2 && parts[1] == "recoveries":
		a.requestRecovery(w, r, name)
	case len(parts) == 3 && parts[1] == "recoveries" && profiles.ID.MatchString(parts[2]):
		a.readRecovery(w, r, name, parts[2])
	case len(parts) == 4 && parts[1] == "recoveries" && profiles.ID.MatchString(parts[2]) && parts[3] == "items":
		a.readRecoveryItems(w, r, name, parts[2])
	default:
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
	}
}

// privacyAdmin authorizes a change to what a subscription authorizes and returns the acting operator.
func (a *API) privacyAdmin(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !allow(w, r, http.MethodPost) {
		return "", false
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return "", false
	}
	operator, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin)
	if !ok {
		return "", false
	}
	if operator.Subject == "" {
		return "development-bearer", true
	}
	return operator.Subject, true
}

func (a *API) readSubscription(w http.ResponseWriter, r *http.Request, read func() (any, error)) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	items, err := read()
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"items": items})
}

func (a *API) createSubscription(w http.ResponseWriter, r *http.Request, name string) {
	actor, ok := a.privacyAdmin(w, r)
	if !ok {
		return
	}
	var body SubscriptionRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.Store.CreateSubscription(r.Context(), name, body, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}

func (a *API) setSubscriptionState(w http.ResponseWriter, r *http.Request, name string) {
	if _, ok := a.privacyAdmin(w, r); !ok {
		return
	}
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
}

// reapproveSubscription approves a new subscription that replaces an interrupted one. It sends nothing.
func (a *API) reapproveSubscription(w http.ResponseWriter, r *http.Request, name string) {
	actor, ok := a.privacyAdmin(w, r)
	if !ok {
		return
	}
	var body ReapprovalRequest
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.Store.ReapproveSubscription(r.Context(), name, body, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}

// requestRecovery queues what the subscription's interrupted predecessor missed, idempotently per key.
func (a *API) requestRecovery(w http.ResponseWriter, r *http.Request, name string) {
	actor, ok := a.privacyAdmin(w, r)
	if !ok {
		return
	}
	key := r.Header.Values("Idempotency-Key")
	if len(key) != 1 || !keyPattern.MatchString(key[0]) {
		writeProblem(w, problem(400, "Invalid idempotency key", "Supply one Idempotency-Key of 16 to 128 letters, digits, hyphens or underscores."))
		return
	}
	result, created, err := a.Store.RequestRecovery(r.Context(), name, key[0], actor)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, result)
}

func (a *API) readRecovery(w http.ResponseWriter, r *http.Request, name, id string) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	result, err := a.Store.Recovery(r.Context(), name, id)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// readRecoveryItems pages through a recovery's events: ?after=<event ID>&limit=<1..500>.
func (a *API) readRecoveryItems(w http.ResponseWriter, r *http.Request, name, id string) {
	if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
		return
	}
	query := r.URL.Query()
	limit := int64(100)
	for key, values := range query {
		var err error
		switch {
		case len(values) != 1:
			err = errors.New("repeated")
		case key == "limit":
			limit, err = strconv.ParseInt(values[0], 10, 32)
			if err == nil && (limit < 1 || limit > 500) {
				err = errors.New("range")
			}
		case key == "after":
			if !profiles.ID.MatchString(values[0]) {
				err = errors.New("format")
			}
		default:
			err = errors.New("unknown")
		}
		if err != nil {
			writeProblem(w, problem(400, "Invalid query", "Use after=<event ID> and limit=1..500."))
			return
		}
	}
	items, err := a.Store.RecoveryItems(r.Context(), name, id, query.Get("after"), int32(limit))
	if err != nil {
		fail(w, err)
		return
	}
	body := map[string]any{"items": items}
	if len(items) == int(limit) {
		body["next"] = items[len(items)-1].EventID
	}
	writeJSON(w, 200, body)
}
