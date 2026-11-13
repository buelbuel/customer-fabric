package platform

import (
	"net/http"
	"net/url"
	"strconv"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	customerConsentPath   = "/v1/customer/consent"
	customerHistoryPath   = customerConsentPath + "/history"
	customerDecisionsPath = customerConsentPath + "/decisions"
)

// serveCustomerConsent exposes only the authenticated subject's consent. No route takes a subject,
// profile or actor, and every resource is resolved from the session.
func (a *API) serveCustomerConsent(w http.ResponseWriter, r *http.Request) {
	method := http.MethodGet
	switch r.URL.Path {
	case customerConsentPath, customerHistoryPath:
	case customerDecisionsPath:
		method = http.MethodPost
	default:
		writeProblem(w, problem(404, "Not found", "Unknown consent resource."))
		return
	}
	if !allow(w, r, method) {
		return
	}
	session, ok := a.authenticateCustomer(w, r)
	if !ok {
		return
	}
	if method == http.MethodPost {
		a.recordCustomerDecision(w, r, session)
		return
	}
	cursor, limit, ok := parsePage(w, r)
	if !ok || !emptyLinkingBody(w, r) {
		return
	}
	var page any
	var err error
	if r.URL.Path == customerConsentPath {
		page, err = a.Store.ListCustomerConsent(r.Context(), session.Subject, cursor, limit)
	} else {
		page, err = a.Store.ListCustomerEvidence(r.Context(), session.Subject, cursor, limit)
	}
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, page)
}

func (a *API) recordCustomerDecision(w http.ResponseWriter, r *http.Request, session customerSession) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	var input struct {
		PolicyVersionID string `json:"policy_version_id"`
		Decision        string `json:"decision"`
	}
	if !decodeLinkingJSON(w, r, &input, "policy_version_id", "decision") {
		return
	}
	decision := consent.Decision(input.Decision)
	if !profiles.ID.MatchString(input.PolicyVersionID) || (decision != consent.Grant && decision != consent.Withdraw) {
		writeProblem(w, problem(422, "Invalid decision", "Use a policy version ID and a decision of grant or withdraw."))
		return
	}
	key := r.Header.Values("Idempotency-Key")
	if len(key) != 1 {
		writeProblem(w, problem(422, "Invalid decision", "Exactly one Idempotency-Key header is required."))
		return
	}
	var revision int64
	if decision == consent.Grant {
		etag := r.Header.Get("If-Match")
		if etag == "" {
			writeProblem(w, problem(428, "Precondition required", "A grant needs the purpose revision in If-Match."))
			return
		}
		var err error
		if versionPattern.MatchString(etag) {
			revision, err = strconv.ParseInt(etag[1:len(etag)-1], 10, 64)
		}
		if !versionPattern.MatchString(etag) || err != nil {
			writeProblem(w, problem(400, "Invalid precondition", "Use a quoted positive revision."))
			return
		}
	}
	channel := "customer_native"
	if session.Browser {
		channel = "customer_browser"
	}
	receipt, err := a.Store.AppendCustomerDecision(r.Context(), CustomerDecision{
		KratosSubject: session.Subject, PolicyVersionID: input.PolicyVersionID, Decision: decision,
		IdempotencyKey: key[0], ProofChannel: channel, ExpectedRevision: revision,
	})
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

// parsePage reads the cursor and limit shared by the customer list routes.
func parsePage(w http.ResponseWriter, r *http.Request) (string, int, bool) {
	params, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeProblem(w, problem(400, "Invalid query", "Query encoding is invalid."))
		return "", 0, false
	}
	for k, values := range params {
		if (k != "cursor" && k != "limit") || len(values) != 1 || values[0] == "" {
			writeProblem(w, problem(400, "Invalid query", "Only cursor and limit are supported."))
			return "", 0, false
		}
	}
	cursor := params.Get("cursor")
	limit := 50
	if params.Has("limit") {
		limit, err = strconv.Atoi(params.Get("limit"))
	}
	if err != nil || limit < 1 || limit > 100 || (cursor != "" && !profiles.ID.MatchString(cursor)) {
		writeProblem(w, problem(400, "Invalid query", "Use a UUID cursor and limit between 1 and 100."))
		return "", 0, false
	}
	return cursor, limit, true
}
