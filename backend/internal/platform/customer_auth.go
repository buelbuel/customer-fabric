package platform

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
)

// customerSession is a verified Kratos subject and the transport that proved it.
type customerSession struct {
	Subject string
	Browser bool
}

// authenticateCustomer is the only customer authenticator (ADR 0009 section 14). A request carries
// exactly one of a native session token or the configured browser cookie. A cookie from a sibling
// checkout is never selected or forwarded, because browsers do not scope cookies by port.
func (a *API) authenticateCustomer(w http.ResponseWriter, r *http.Request) (customerSession, bool) {
	tokens := r.Header.Values("X-Session-Token")
	var cookies []*http.Cookie
	if origin := a.Config.CustomerBrowserOrigin; origin != "" {
		cookies = r.CookiesNamed("cf_customer_" + strings.TrimPrefix(origin, "http://127.0.0.1:"))
	}
	native := len(tokens) == 1 && len(r.Header.Values("Cookie")) == 0
	browser := len(tokens) == 0 && len(cookies) == 1
	if len(r.Header.Values("Authorization")) != 0 || (!native && !browser) {
		writeProblem(w, problem(401, "Unauthorized", "A customer session is required."))
		return customerSession{}, false
	}
	if browser && !a.allowBrowserWrite(w, r) {
		return customerSession{}, false
	}
	var subject string
	var err error
	if browser {
		subject, err = a.kratos.VerifiedBrowserSubject(r.Context(), cookies[0])
	} else {
		subject, err = a.kratos.VerifiedSubject(r.Context(), tokens[0])
	}
	switch {
	case errors.Is(err, identity.ErrInvalidSession):
		writeProblem(w, problem(401, "Unauthorized", "A valid customer session is required."))
		return customerSession{}, false
	case errors.Is(err, identity.ErrUnverifiedAddress):
		writeProblem(w, problem(403, "Verification required", "Verify the account email before accessing customer resources."))
		return customerSession{}, false
	case err != nil:
		writeProblem(w, problem(503, "Identity service unavailable", "Customer session verification could not be completed."))
		return customerSession{}, false
	}
	return customerSession{Subject: subject, Browser: browser}, true
}

// allowBrowserWrite applies the cross-site request checks to a cookie-authenticated state change.
// It runs before the session is verified, so a forged request never reaches Kratos.
func (a *API) allowBrowserWrite(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	origins, sites := r.Header.Values("Origin"), r.Header.Values("Sec-Fetch-Site")
	if len(origins) != 1 || origins[0] != a.Config.CustomerBrowserOrigin ||
		(len(sites) != 0 && (len(sites) != 1 || sites[0] != "same-origin")) {
		writeProblem(w, problem(403, "Origin rejected", "Browser state changes require the configured origin."))
		return false
	}
	if r.ContentLength != 0 {
		if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
			writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
			return false
		}
	}
	return true
}
