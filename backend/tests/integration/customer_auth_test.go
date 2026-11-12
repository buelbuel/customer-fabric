package integration

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const (
	customerOrigin = "http://127.0.0.1:4433"
	customerCookie = "cf_customer_4433"
	siblingCookie  = "cf_customer_12345"
	customerRoute  = "/v1/customer/profile"
)

// whoamiFixture isolates database behavior from account setup. Live Kratos is covered by
// make m1-contracts and the M1 smokes. It records what the API forwarded.
type whoamiFixture struct {
	mu       sync.Mutex
	sessions map[string]whoamiSession
	calls    int
	cookies  []string
}

type whoamiSession struct {
	kratos, state string
}

func (f *whoamiFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	key := r.Header.Get("X-Session-Token")
	for _, c := range r.Cookies() {
		f.cookies = append(f.cookies, c.Name)
		key = c.Value
	}
	session, ok := f.sessions[key]
	switch {
	case !ok:
		w.WriteHeader(401)
		return
	case session.state == "outage":
		w.WriteHeader(500)
		return
	}
	expires, active, identityState, verified := time.Now().Add(time.Hour), true, "active", true
	switch session.state {
	case "expired":
		expires = time.Now().Add(-time.Minute)
	case "revoked":
		active = false
	case "inactive":
		identityState = "inactive"
	case "unverified":
		verified = false
	}
	fmt.Fprintf(w, `{"active":%t,"expires_at":%q,"identity":{"id":%q,"state":%q,"traits":{"email":"customer@example.test"},"verifiable_addresses":[{"via":"email","value":"customer@example.test","verified":%t}]}}`,
		active, expires.Format(time.RFC3339), session.kratos, identityState, verified)
}

func (f *whoamiFixture) reset() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls, f.cookies = 0, nil
}

type customerAPI struct {
	*consentSuite
	whoami *whoamiFixture
}

func customerSetup(t *testing.T) *customerAPI {
	t.Helper()
	s := consentSetup(t)
	whoami := &whoamiFixture{sessions: map[string]whoamiSession{}}
	server := httptest.NewServer(whoami)
	t.Cleanup(server.Close)
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{
		Mode: "development", OperatorKratosPublicURL: newOperatorIssuer(t), KratosPublicURL: server.URL,
		CustomerBrowserOrigin: customerOrigin, Tokens: map[string]string{"console": tokenA, "importer": tokenB},
	})
	if err != nil {
		t.Fatal(err)
	}
	s.h = h
	return &customerAPI{s, whoami}
}

// session registers a credential usable as either a native token or a cookie value.
func (s *customerAPI) session(kratos, state string) string {
	credential := "ory_st_" + strings.ReplaceAll(platform.UUID(), "-", "")
	s.whoami.sessions[credential] = whoamiSession{kratos, state}
	return credential
}

type credential func(*http.Request)

func nativeSession(token string) credential {
	return func(r *http.Request) { r.Header.Set("X-Session-Token", token) }
}

func browserCookie(value string) credential {
	return func(r *http.Request) { r.AddCookie(&http.Cookie{Name: customerCookie, Value: value}) }
}

// browserSession is a cookie request as the same-origin reference sends it.
func browserSession(value string) credential {
	return func(r *http.Request) {
		browserCookie(value)(r)
		r.Header.Set("Origin", customerOrigin)
		r.Header.Set("Sec-Fetch-Site", "same-origin")
	}
}

func (s *customerAPI) send(method, path, body string, credentials ...credential) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		r.Header.Set("Content-Type", "application/json")
	}
	for _, apply := range credentials {
		apply(r)
	}
	w := httptest.NewRecorder()
	s.h.ServeHTTP(w, r)
	return w
}

func TestCustomerAuthenticatorRequiresExactlyOneCredential(t *testing.T) {
	s := customerSetup(t)
	session := s.session(platform.UUID(), "active")
	extraCookie := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: "unrelated", Value: "synthetic"}) }
	bearer := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+session) }
	sibling := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: siblingCookie, Value: session}) }
	for name, credentials := range map[string][]credential{
		"none":                     nil,
		"both credentials":         {nativeSession(session), browserCookie(session)},
		"native with other cookie": {nativeSession(session), extraCookie},
		"native with bearer":       {nativeSession(session), bearer},
		"cookie with bearer":       {browserCookie(session), bearer},
		"sibling cookie only":      {sibling},
		"repeated cookie":          {browserCookie(session), browserCookie(session)},
	} {
		for _, path := range []string{customerRoute, associations} {
			t.Run(name+" "+path, func(t *testing.T) {
				s.whoami.reset()
				if w := s.send("GET", path, "", credentials...); w.Code != 401 {
					t.Fatalf("status %d", w.Code)
				}
				if s.whoami.calls != 0 {
					t.Fatal("an ambiguous credential reached Kratos")
				}
			})
		}
	}
}

func TestCustomerAuthenticatorAcceptsEitherTransportOnEveryRoute(t *testing.T) {
	s := customerSetup(t)
	session := s.session(platform.UUID(), "active")
	s.expect(s.send("PUT", customerRoute, "", browserSession(session)), 200)
	for _, path := range []string{customerRoute, associations} {
		s.expect(s.send("GET", path, "", nativeSession(session)), 200)
		s.expect(s.send("GET", path, "", browserCookie(session)), 200)
	}
}

func TestSiblingCookieIsNeverForwarded(t *testing.T) {
	s := customerSetup(t)
	session := s.session(platform.UUID(), "active")
	s.expect(s.send("PUT", customerRoute, "", nativeSession(session)), 200)
	s.whoami.reset()
	sibling := func(r *http.Request) { r.AddCookie(&http.Cookie{Name: siblingCookie, Value: "synthetic"}) }
	s.expect(s.send("GET", customerRoute, "", browserCookie(session), sibling), 200)
	if len(s.whoami.cookies) != 1 || s.whoami.cookies[0] != customerCookie {
		t.Fatalf("forwarded %v", s.whoami.cookies)
	}
}

func TestCustomerAuthenticatorRejectsUnusableSessions(t *testing.T) {
	s := customerSetup(t)
	for state, status := range map[string]int{"expired": 401, "revoked": 401, "inactive": 401, "unverified": 403, "outage": 503} {
		session := s.session(platform.UUID(), state)
		for _, path := range []string{customerRoute, associations} {
			for name, apply := range map[string]credential{"native": nativeSession(session), "browser": browserCookie(session)} {
				t.Run(state+" "+name+" "+path, func(t *testing.T) {
					if w := s.send("GET", path, "", apply); w.Code != status {
						t.Fatalf("status %d", w.Code)
					}
				})
			}
		}
	}
}

func TestBrowserWritesRejectCrossSiteRequests(t *testing.T) {
	s := customerSetup(t)
	session := s.session(platform.UUID(), "active")
	set := func(name string, values ...string) credential {
		return func(r *http.Request) { r.Header[name] = values }
	}
	for name, overrides := range map[string][]credential{
		"missing origin":   {func(r *http.Request) { r.Header.Del("Origin") }},
		"other origin":     {set("Origin", "https://attacker.example")},
		"sibling origin":   {set("Origin", "http://127.0.0.1:12345")},
		"null origin":      {set("Origin", "null")},
		"repeated origin":  {set("Origin", customerOrigin, customerOrigin)},
		"cross-site fetch": {set("Sec-Fetch-Site", "cross-site")},
		"same-site fetch":  {set("Sec-Fetch-Site", "same-site")},
		"no-context fetch": {set("Sec-Fetch-Site", "none")},
		"repeated fetch":   {set("Sec-Fetch-Site", "same-origin", "same-origin")},
	} {
		for _, route := range []struct{ method, path, body string }{
			{"PUT", customerRoute, ""},
			{"POST", associations, `{"grant":"synthetic"}`},
			{"DELETE", associations + "/" + platform.UUID(), ""},
		} {
			t.Run(name+" "+route.method+" "+route.path, func(t *testing.T) {
				s.whoami.reset()
				if w := s.send(route.method, route.path, route.body, append([]credential{browserSession(session)}, overrides...)...); w.Code != 403 {
					t.Fatalf("status %d", w.Code)
				}
				if s.whoami.calls != 0 {
					t.Fatal("a forged write reached Kratos")
				}
			})
		}
	}
}

func TestBrowserWritesRequireJSONBodies(t *testing.T) {
	s := customerSetup(t)
	session := s.session(platform.UUID(), "active")
	s.expect(s.send("PUT", customerRoute, "", browserSession(session)), 200)
	for _, media := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		w := s.send("POST", associations, `{"grant":"synthetic"}`, browserSession(session), func(r *http.Request) { r.Header.Set("Content-Type", media) })
		s.expect(w, 415)
	}
	// Past every browser check, the request is decided by the grant itself.
	s.expect(s.send("POST", associations, `{"grant":"synthetic"}`, browserSession(session)), 409)
	// A browser that omits Sec-Fetch-Site is still bound by the exact Origin.
	s.expect(s.send("PUT", customerRoute, "", browserSession(session), func(r *http.Request) { r.Header.Del("Sec-Fetch-Site") }), 200)
}

func TestNativeWritesAreNotBrowserChecked(t *testing.T) {
	s := customerSetup(t)
	session := s.session(platform.UUID(), "active")
	s.expect(s.send("PUT", customerRoute, "", nativeSession(session), func(r *http.Request) { r.Header.Set("Origin", "https://app.example") }), 200)
}
