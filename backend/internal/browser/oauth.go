package browser

import (
	"bytes"
	"encoding/json"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"slices"
	"strings"
)

// The OAuth scopes Customer Fabric grants (ADR 0012). OAuth consent lets an application act for the
// customer; it is never evidence for a processing purpose, which stays in the consent registry.
var oauthScopes = map[string]string{
	"openid":         "Sign you in with your Customer Fabric account",
	"offline_access": "Stay signed in without asking you again",
	"email":          "See your verified email address",
	"profile.read":   "Read your display name, preferred language and time zone",
	"profile.write":  "Change your display name, preferred language and time zone",
}

// apiAudience is the only resource an application may request tokens for.
const apiAudience = "customer-fabric-api"

// challengeChars are printable ASCII without spaces. Hydra's challenges are opaque, can exceed a thousand
// characters and include padding; they only ever go back to Hydra as an encoded query parameter.
var challengeChars = regexp.MustCompile(`^[!-~]+$`)

func validChallenge(v string) bool { return len(v) <= 4096 && challengeChars.MatchString(v) }

// oauth bridges Hydra to Kratos: Hydra delegates login and consent here, and the customer's existing
// Kratos session decides who is signing in.
type oauth struct {
	admin  *url.URL
	public *httputil.ReverseProxy
}

// hydraRoute reports Hydra's public endpoints, served on the customer origin so it is also the issuer.
func hydraRoute(method, path string) bool {
	switch path {
	case "/oauth2/auth", "/oauth2/sessions/logout":
		return method == http.MethodGet || method == http.MethodPost
	case "/oauth2/token", "/oauth2/revoke":
		return method == http.MethodPost
	case "/userinfo":
		return method == http.MethodGet || method == http.MethodPost
	case "/.well-known/openid-configuration", "/.well-known/jwks.json":
		return method == http.MethodGet
	}
	return false
}

func newOAuth(adminURL, publicURL string) (*oauth, error) {
	admin, err := upstream(adminURL)
	if err != nil {
		return nil, err
	}
	public, err := upstream(publicURL)
	if err != nil {
		return nil, err
	}
	return &oauth{admin: admin, public: &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(public)
			p.Out.Host = p.In.Host
			// Only Hydra's own cookies reach Hydra; the customer's Kratos session never does.
			p.Out.Header.Del("Cookie")
			for _, c := range p.In.Cookies() {
				if strings.HasPrefix(c.Name, "ory_hydra") || strings.HasPrefix(c.Name, "oauth2_") {
					p.Out.AddCookie(c)
				}
			}
			p.Out.Header.Set("X-Forwarded-Proto", "http")
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Authorization service unavailable.", http.StatusServiceUnavailable)
		},
	}}, nil
}

// adminJSON calls Hydra's admin API.
func (g *gateway) adminJSON(r *http.Request, method, path string, query url.Values, body any, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	}
	target := *g.oauth.admin
	target.Path, target.RawQuery = path, query.Encode()
	req, err := http.NewRequestWithContext(r.Context(), method, target.String(), payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return g.decode(req, out)
}

// customerIdentity is the signed-in customer according to Kratos, if any.
type customerIdentity struct {
	ID       string
	Email    string
	Verified bool
}

func (g *gateway) whoami(r *http.Request) (customerIdentity, int, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, g.kratos.String()+"/sessions/whoami", nil)
	if err != nil {
		return customerIdentity{}, 0, err
	}
	req.Header.Set("Accept", "application/json")
	g.copyCookies(r, req)
	var session struct {
		Active   bool `json:"active"`
		Identity struct {
			ID     string `json:"id"`
			Traits struct {
				Email string `json:"email"`
			} `json:"traits"`
			Addresses []struct {
				Value    string `json:"value"`
				Verified bool   `json:"verified"`
			} `json:"verifiable_addresses"`
		} `json:"identity"`
	}
	status, err := g.decode(req, &session)
	if err != nil || status != http.StatusOK || !session.Active {
		return customerIdentity{}, status, err
	}
	who := customerIdentity{ID: session.Identity.ID, Email: session.Identity.Traits.Email}
	for _, a := range session.Identity.Addresses {
		who.Verified = who.Verified || (a.Verified && strings.EqualFold(a.Value, who.Email))
	}
	return who, status, nil
}

// oauthLogin accepts Hydra's login request for the customer already signed in to Kratos, or sends the
// browser to Kratos login and back. An unverified address cannot sign in to an application.
func (g *gateway) oauthLogin(w http.ResponseWriter, r *http.Request) {
	login := r.URL.Query().Get("login_challenge")
	if !validChallenge(login) {
		http.Error(w, "Invalid login request.", http.StatusBadRequest)
		return
	}
	var request struct {
		Skip    bool   `json:"skip"`
		Subject string `json:"subject"`
	}
	if status, err := g.adminJSON(r, http.MethodGet, "/admin/oauth2/auth/requests/login", url.Values{"login_challenge": {login}}, nil, &request); err != nil || status != http.StatusOK {
		http.Error(w, "The sign-in request expired. Start again from the application.", http.StatusBadRequest)
		return
	}
	who, status, err := g.whoami(r)
	switch {
	case err != nil || status >= 500:
		http.Error(w, "Identity service unavailable.", http.StatusServiceUnavailable)
		return
	case status != http.StatusOK:
		back := "http://" + r.Host + "/account/oauth/login?" + url.Values{"login_challenge": {login}}.Encode()
		http.Redirect(w, r, "/self-service/login/browser?"+url.Values{"return_to": {back}}.Encode(), http.StatusSeeOther)
		return
	case !who.Verified:
		g.render(w, page{Title: "Verify your email", Message: "Verify your email address before signing in to applications.", Profile: true})
		return
	case request.Skip && request.Subject != who.ID:
		g.reject(w, r, "login", login, "A different account is signed in.")
		return
	}
	var accepted struct {
		RedirectTo string `json:"redirect_to"`
	}
	body := map[string]any{"subject": who.ID, "remember": true, "remember_for": 3600}
	if status, err := g.adminJSON(r, http.MethodPut, "/admin/oauth2/auth/requests/login/accept", url.Values{"login_challenge": {login}}, body, &accepted); err != nil || status != http.StatusOK {
		http.Error(w, "The sign-in could not be completed.", http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, accepted.RedirectTo, http.StatusFound)
}

type consentRequest struct {
	Skip           bool     `json:"skip"`
	Subject        string   `json:"subject"`
	RequestedScope []string `json:"requested_scope"`
	Audience       []string `json:"requested_access_token_audience"`
	Client         struct {
		ID   string `json:"client_id"`
		Name string `json:"client_name"`
	} `json:"client"`
}

var oauthConsentPage = template.Must(template.New("consent").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>Allow {{.App}}?</title></head><body><main>
<h1>Allow {{.App}} to use your account?</h1>
<p>{{.App}} is asking to:</p>
<ul>{{range .Scopes}}<li>{{.}}</li>{{end}}</ul>
<p>This lets the application act for you. It is not a privacy or marketing choice; manage those under <a href="/account/consent">Privacy choices</a>.</p>
<form action="/account/oauth/consent" method="post">
<input type="hidden" name="consent_challenge" value="{{.Challenge}}">
<button type="submit" name="decision" value="allow">Allow</button>
<button type="submit" name="decision" value="deny">Deny</button>
</form></main></body></html>`))

// oauthConsent shows the application's request, or completes it when the customer granted it before.
func (g *gateway) oauthConsent(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "strict-origin")
	if r.Method == http.MethodPost {
		g.submitOAuthConsent(w, r)
		return
	}
	id := r.URL.Query().Get("consent_challenge")
	request, ok := g.consentRequest(w, r, id)
	if !ok {
		return
	}
	if request.Skip {
		g.acceptConsent(w, r, id, request)
		return
	}
	scopes := []string{}
	for _, s := range request.RequestedScope {
		if text, known := oauthScopes[s]; known {
			scopes = append(scopes, text)
		}
	}
	app := request.Client.Name
	if app == "" {
		app = request.Client.ID
	}
	_ = oauthConsentPage.Execute(w, map[string]any{"App": app, "Scopes": scopes, "Challenge": id})
}

// consentRequest loads Hydra's consent request and checks it belongs to the signed-in customer.
func (g *gateway) consentRequest(w http.ResponseWriter, r *http.Request, id string) (consentRequest, bool) {
	var request consentRequest
	if !validChallenge(id) {
		http.Error(w, "Invalid consent request.", http.StatusBadRequest)
		return request, false
	}
	if status, err := g.adminJSON(r, http.MethodGet, "/admin/oauth2/auth/requests/consent", url.Values{"consent_challenge": {id}}, nil, &request); err != nil || status != http.StatusOK {
		http.Error(w, "The request expired. Start again from the application.", http.StatusBadRequest)
		return request, false
	}
	who, status, err := g.whoami(r)
	if err != nil || status != http.StatusOK || who.ID != request.Subject {
		http.Error(w, "Sign in as the account that started this request.", http.StatusForbidden)
		return request, false
	}
	return request, true
}

func (g *gateway) submitOAuthConsent(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(w, "Origin rejected.", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid consent form.", http.StatusBadRequest)
		return
	}
	id, decision := r.PostForm.Get("consent_challenge"), r.PostForm.Get("decision")
	request, ok := g.consentRequest(w, r, id)
	if !ok {
		return
	}
	switch decision {
	case "allow":
		g.acceptConsent(w, r, id, request)
	case "deny":
		g.reject(w, r, "consent", id, "The customer denied the request.")
	default:
		http.Error(w, "Invalid consent form.", http.StatusBadRequest)
	}
}

var continuePage = template.Must(template.New("continue").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta http-equiv="refresh" content="0;url={{.}}">
<title>Returning to the application</title></head><body><main>
<p><a href="{{.}}">Continue to the application</a></p></main></body></html>`))

// finish sends the browser on after a decision. A form submission's redirects are bound by the page's
// form-action policy, which must not name application origins, so a submitted form continues by an
// ordinary navigation instead.
func finish(w http.ResponseWriter, r *http.Request, target string) {
	if r.Method != http.MethodPost {
		http.Redirect(w, r, target, http.StatusFound)
		return
	}
	_ = continuePage.Execute(w, target)
}

// acceptConsent grants only known scopes and only the Customer Fabric API audience. The ID token carries
// the email address only when the email scope is granted.
func (g *gateway) acceptConsent(w http.ResponseWriter, r *http.Request, id string, request consentRequest) {
	scopes := []string{}
	for _, s := range request.RequestedScope {
		if _, known := oauthScopes[s]; known {
			scopes = append(scopes, s)
		}
	}
	audience := []string{}
	if slices.Contains(request.Audience, apiAudience) {
		audience = append(audience, apiAudience)
	}
	claims := map[string]any{}
	if slices.Contains(scopes, "email") {
		who, _, err := g.whoami(r)
		if err == nil && who.Verified {
			claims["email"], claims["email_verified"] = who.Email, true
		}
	}
	body := map[string]any{"grant_scope": scopes, "grant_access_token_audience": audience, "remember": true, "remember_for": 0,
		"session": map[string]any{"id_token": claims}}
	var accepted struct {
		RedirectTo string `json:"redirect_to"`
	}
	if status, err := g.adminJSON(r, http.MethodPut, "/admin/oauth2/auth/requests/consent/accept", url.Values{"consent_challenge": {id}}, body, &accepted); err != nil || status != http.StatusOK {
		http.Error(w, "The request could not be completed.", http.StatusBadGateway)
		return
	}
	finish(w, r, accepted.RedirectTo)
}

// reject ends a login or consent request with access_denied and returns the browser to the application.
func (g *gateway) reject(w http.ResponseWriter, r *http.Request, kind, id, description string) {
	var rejected struct {
		RedirectTo string `json:"redirect_to"`
	}
	body := map[string]string{"error": "access_denied", "error_description": description}
	path := "/admin/oauth2/auth/requests/" + kind + "/reject"
	if status, err := g.adminJSON(r, http.MethodPut, path, url.Values{kind + "_challenge": {id}}, body, &rejected); err != nil || status != http.StatusOK {
		http.Error(w, "The request could not be completed.", http.StatusBadGateway)
		return
	}
	finish(w, r, rejected.RedirectTo)
}

// oauthLogout completes an application-initiated logout of the OAuth session.
func (g *gateway) oauthLogout(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("logout_challenge")
	if !validChallenge(id) {
		http.Error(w, "Invalid logout request.", http.StatusBadRequest)
		return
	}
	var accepted struct {
		RedirectTo string `json:"redirect_to"`
	}
	if status, err := g.adminJSON(r, http.MethodPut, "/admin/oauth2/auth/requests/logout/accept", url.Values{"logout_challenge": {id}}, nil, &accepted); err != nil || status != http.StatusOK {
		http.Error(w, "The logout request expired.", http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, accepted.RedirectTo, http.StatusFound)
}
