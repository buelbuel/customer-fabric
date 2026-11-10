// Package browser renders Kratos browser flows for the local reference UI.
package browser

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var loopback = regexp.MustCompile(`^127\.0\.0\.1:[0-9]{1,5}$`)

var flowID = regexp.MustCompile(`^[0-9a-f-]{36}$`)

type gateway struct {
	kratos     *url.URL
	api        *url.URL
	proxy      *httputil.ReverseProxy
	cookieName string
	operator   bool
	client     *http.Client
	oauth      *oauth
}

func upstream(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.Path != "" ||
		u.RawQuery != "" || u.Fragment != "" || u.ForceQuery {
		return nil, errors.New("expected an internal HTTP origin")
	}
	return u, nil
}

func New(kratosURL, apiURL, cookieName string) (http.Handler, error) {
	return newGateway(kratosURL, apiURL, cookieName, false)
}

// NewCustomer is the customer gateway with Hydra's login, consent and public endpoints on its origin.
func NewCustomer(kratosURL, apiURL, cookieName, hydraAdminURL, hydraPublicURL string) (http.Handler, error) {
	handler, err := newGateway(kratosURL, apiURL, cookieName, false)
	if err != nil || hydraAdminURL == "" {
		return handler, err
	}
	g := handler.(*gateway)
	if g.oauth, err = newOAuth(hydraAdminURL, hydraPublicURL); err != nil {
		return nil, err
	}
	g.oauth.public.Transport = g.client.Transport
	return g, nil
}

func NewOperator(kratosURL, apiURL string) (http.Handler, error) {
	if apiURL == "" {
		return nil, errors.New("operator reference requires a profile API origin")
	}
	return newGateway(kratosURL, apiURL, "", true)
}

func newGateway(kratosURL, apiURL, cookieName string, operator bool) (http.Handler, error) {
	kratos, err := upstream(kratosURL)
	if err != nil {
		return nil, err
	}
	var api *url.URL
	if apiURL != "" {
		api, err = upstream(apiURL)
		if err != nil {
			return nil, err
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.ResponseHeaderTimeout = 5 * time.Second
	g := &gateway{
		kratos: kratos, api: api, cookieName: cookieName, operator: operator,
		client: &http.Client{
			Transport: transport, Timeout: 6 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	g.proxy = &httputil.ReverseProxy{
		Rewrite: func(p *httputil.ProxyRequest) {
			p.SetURL(kratos)
			p.Out.Header.Del("Cookie")
			g.copyCookies(p.In, p.Out)
		},
		Transport: transport,
		ErrorLog:  log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "Identity service unavailable.", http.StatusServiceUnavailable)
		},
	}
	return g, nil
}

var flows = map[string]string{
	"registration": "Register", "login": "Log in", "verification": "Verify email",
	"recovery": "Recover account", "settings": "Account settings",
}

func publicRoute(method, path string) bool {
	if path == "/sessions/whoami" || path == "/health/ready" {
		return method == http.MethodGet
	}
	if path == "/self-service/logout" || path == "/self-service/logout/browser" {
		return method == http.MethodGet
	}
	if path == "/self-service/logout/api" {
		return method == http.MethodDelete
	}
	for flow := range flows {
		base := "/self-service/" + flow
		switch path {
		case base:
			return method == http.MethodPost
		case base + "/api", base + "/browser", base + "/flows":
			return method == http.MethodGet
		}
	}
	return false
}

func checkResponse(response *http.Response, host string) error {
	if location := response.Header.Get("Location"); location != "" {
		target, err := url.Parse(location)
		if err != nil || target.User != nil || target.Opaque != "" ||
			(target.IsAbs() && (target.Scheme != "http" || target.Host != host)) ||
			(!target.IsAbs() && (target.Host != "" || !strings.HasPrefix(target.Path, "/"))) ||
			strings.Contains(target.Path, "\\") || strings.HasPrefix(target.Path, "//") {
			return errors.New("identity redirect rejected")
		}
	}
	response.Header.Set("Cache-Control", "no-store")
	response.Header.Set("Referrer-Policy", "no-referrer")
	return nil
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src data:; form-action 'self'; base-uri 'none'; frame-ancestors 'none'")
	if !loopback.MatchString(r.Host) || r.URL.RawPath != "" {
		http.Error(w, "Invalid reference origin or path.", http.StatusBadRequest)
		return
	}
	if g.oauth != nil && hydraRoute(r.Method, r.URL.Path) {
		w.Header().Del("Content-Type")
		w.Header().Del("Content-Security-Policy")
		g.oauth.public.ServeHTTP(w, r)
		return
	}
	if g.oauth != nil && strings.HasPrefix(r.URL.Path, "/account/oauth/") {
		switch {
		case r.URL.Path == "/account/oauth/login" && r.Method == http.MethodGet:
			g.oauthLogin(w, r)
		case r.URL.Path == "/account/oauth/consent" && (r.Method == http.MethodGet || r.Method == http.MethodPost):
			g.oauthConsent(w, r)
		case r.URL.Path == "/account/oauth/logout" && r.Method == http.MethodGet:
			g.oauthLogout(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}
	if publicRoute(r.Method, r.URL.Path) {
		r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
		proxy := *g.proxy
		proxy.ModifyResponse = func(response *http.Response) error {
			return checkResponse(response, r.Host)
		}
		proxy.ServeHTTP(w, r)
		return
	}
	if g.operator && r.URL.Path == "/account/operator-profiles" && r.Method == http.MethodGet {
		g.operatorProfiles(w, r)
		return
	}
	if !g.operator && r.URL.Path == "/account/profile" && g.api != nil &&
		(r.Method == http.MethodGet || r.Method == http.MethodPost) {
		g.profile(w, r)
		return
	}
	if !g.operator && r.URL.Path == "/account/preferences" && g.api != nil &&
		(r.Method == http.MethodGet || r.Method == http.MethodPost) {
		g.preferences(w, r)
		return
	}
	if !g.operator && r.URL.Path == "/account/consent" && g.api != nil &&
		(r.Method == http.MethodGet || r.Method == http.MethodPost) {
		g.consent(w, r)
		return
	}
	if r.Method != http.MethodGet {
		http.NotFound(w, r)
		return
	}
	switch r.URL.Path {
	case "/", "/account/error":
		p := page{Title: "Customer Fabric identity reference", Profile: g.api != nil}
		if r.URL.Path == "/account/error" {
			p.Message = "The identity flow failed or expired. Start a new flow."
		}
		g.render(w, p)
	case "/account/logout":
		g.logout(w, r)
	default:
		name := strings.TrimPrefix(r.URL.Path, "/account/")
		if _, ok := flows[name]; ok && r.URL.Path == "/account/"+name {
			g.showFlow(w, r, name)
		} else {
			http.NotFound(w, r)
		}
	}
}

func (g *gateway) sessionCookie(host string) string {
	if g.cookieName != "" {
		return g.cookieName
	}
	port := strings.TrimPrefix(host, "127.0.0.1:")
	if g.operator {
		return "cf_operator_" + port
	}
	return "cf_customer_" + port
}

func (g *gateway) copyCookies(in, out *http.Request) {
	for _, c := range in.Cookies() {
		if (strings.HasPrefix(c.Name, "cf_customer_") || strings.HasPrefix(c.Name, "cf_operator_")) &&
			c.Name != g.sessionCookie(in.Host) {
			continue
		}
		out.AddCookie(c)
	}
}

func (g *gateway) get(ctx context.Context, r *http.Request, host *url.URL, path string, query url.Values, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, host.String()+path+"?"+query.Encode(), nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	g.copyCookies(r, req)
	return g.decode(req, out)
}

func (g *gateway) decode(req *http.Request, out any) (int, error) {
	res, err := g.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return res.StatusCode, nil
	}
	const max = 512 << 10
	raw, err := io.ReadAll(io.LimitReader(res.Body, max+1))
	if err != nil || len(raw) > max {
		return 0, errors.New("invalid upstream response")
	}
	return res.StatusCode, json.Unmarshal(raw, out)
}

type field struct {
	Name, Type, Value, Label string
	Disabled, Required       bool
}

type page struct {
	Operator                                                         bool
	Title, Message, Action, Restart, Logout, ProfileJSON, TOTPSecret string
	Forms                                                            [][]field
	Profile                                                          bool
	Consent                                                          *consentView
}

func localAction(r *http.Request, value, path string) string {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.Fragment != "" || u.Path != path || u.RawPath != "" ||
		(u.IsAbs() && (u.Scheme != "http" || u.Host != r.Host)) ||
		(!u.IsAbs() && u.Host != "") {
		return ""
	}
	return u.RequestURI()
}

func (g *gateway) showFlow(w http.ResponseWriter, r *http.Request, name string) {
	base := "/self-service/" + name
	p := page{Title: flows[name], Restart: base + "/browser", Profile: g.api != nil}
	id := r.URL.Query().Get("flow")
	if !flowID.MatchString(id) {
		w.WriteHeader(http.StatusBadRequest)
		p.Message = "Start a new flow."
		g.render(w, p)
		return
	}
	type message struct {
		Type string
	}
	var f struct {
		ExpiresAt time.Time `json:"expires_at"`
		Type      string
		UI        struct {
			Action, Method string
			Messages       []message
			Nodes          []struct {
				Type, Group string
				Messages    []message
				Attributes  struct {
					Name, Type string
					ID         string
					Text       struct{ Text string }
					Value      json.RawMessage
					Disabled   bool
					Required   bool
				}
				Meta struct{ Label struct{ Text string } }
			}
		}
	}
	status, err := g.get(r.Context(), r, g.kratos, base+"/flows", url.Values{"id": {id}}, &f)
	switch {
	case err != nil || status >= 500:
		w.WriteHeader(http.StatusServiceUnavailable)
		p.Message = "Identity service unavailable."
	case status != 200 || f.Type != "browser" || !f.ExpiresAt.After(time.Now()):
		w.WriteHeader(http.StatusBadRequest)
		p.Message = "This browser flow is invalid, expired or no longer available. Start a new flow."
	default:
		p.Action = localAction(r, f.UI.Action, base)
		if p.Action == "" || !strings.EqualFold(f.UI.Method, http.MethodPost) {
			w.WriteHeader(http.StatusBadGateway)
			p.Message = "Invalid identity flow action."
			break
		}
		if len(f.UI.Messages) != 0 {
			p.Message = "Review the flow status. If a code was requested, check your email."
			for _, m := range f.UI.Messages {
				if m.Type == "error" {
					p.Message = "The identity service rejected the submission. Review the fields and try again."
					break
				}
			}
		}
		var common []field
		var resend *field
		groups := map[string][]field{}
		var order []string
		for _, n := range f.UI.Nodes {
			a := n.Attributes
			if name == "settings" && n.Type == "text" && a.ID == "totp_secret_key" {
				p.TOTPSecret = a.Text.Text
			}
			switch a.Type {
			case "hidden", "email", "text", "password", "submit", "number", "checkbox":
			default:
				continue
			}
			if n.Type != "input" || a.Name == "" {
				continue
			}
			node := field{Name: a.Name, Type: a.Type, Label: n.Meta.Label.Text, Disabled: a.Disabled, Required: a.Required}
			if a.Type != "password" && a.Name != "code" && a.Name != "totp_code" {
				_ = json.Unmarshal(a.Value, &node.Value)
			}
			if node.Label == "" {
				node.Label = a.Name
			}
			for _, m := range n.Messages {
				if m.Type == "error" {
					node.Label += " (check this value)"
					p.Message = "The identity service rejected a value. Review the fields and try again."
					break
				}
			}
			if a.Type == "submit" && a.Name == "email" && (name == "verification" || name == "recovery") {
				resend = &node
				continue
			}
			if n.Group == "default" {
				common = append(common, node)
				continue
			}
			if _, exists := groups[n.Group]; !exists {
				order = append(order, n.Group)
			}
			groups[n.Group] = append(groups[n.Group], node)
		}
		for _, group := range order {
			fields := append([]field{}, common...)
			p.Forms = append(p.Forms, append(fields, groups[group]...))
		}
		if len(order) == 0 && len(common) != 0 {
			p.Forms = [][]field{common}
		}
		if resend != nil {
			// Kratos treats sending an address and redeeming a code as distinct submissions.
			fields := append([]field{}, common...)
			p.Forms = append(p.Forms, append(fields, field{Name: "method", Type: "hidden", Value: "code"}, *resend))
		}
	}
	g.render(w, p)
}

func (g *gateway) logout(w http.ResponseWriter, r *http.Request) {
	p := page{Title: "Log out", Profile: g.api != nil}
	var f struct {
		URL string `json:"logout_url"`
	}
	status, err := g.get(r.Context(), r, g.kratos, "/self-service/logout/browser", nil, &f)
	if err != nil || status >= 500 {
		w.WriteHeader(http.StatusServiceUnavailable)
		p.Message = "Identity service unavailable."
	} else if p.Logout = localAction(r, f.URL, "/self-service/logout"); p.Logout == "" {
		p.Message = "No active browser session."
	}
	g.render(w, p)
}

// authorizeAPI forwards only this origin's session and the headers the API uses to reject cross-site writes.
func (g *gateway) authorizeAPI(in, out *http.Request) {
	out.Header.Set("Origin", "http://"+in.Host)
	if site := in.Header.Values("Sec-Fetch-Site"); len(site) != 0 {
		out.Header["Sec-Fetch-Site"] = site
	}
	for _, cookie := range in.CookiesNamed(g.sessionCookie(in.Host)) {
		out.AddCookie(cookie)
	}
}

func (g *gateway) profile(w http.ResponseWriter, r *http.Request) {
	// Browsers can send Origin: null on a form submitted from no-referrer pages.
	// An origin-only policy avoids leaking paths and lets us enforce exact Origin.
	w.Header().Set("Referrer-Policy", "strict-origin")
	p := page{Title: "Current customer profile", Profile: true}
	method := http.MethodGet
	if r.Method == http.MethodPost {
		if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != "http://"+r.Host {
			http.Error(w, "Origin rejected.", http.StatusForbidden)
			return
		}
		method = http.MethodPut
	}
	req, err := http.NewRequestWithContext(r.Context(), method, g.api.String()+"/v1/customer/profile", nil)
	if err != nil {
		http.Error(w, "Invalid profile configuration.", http.StatusServiceUnavailable)
		return
	}
	g.authorizeAPI(r, req)
	var profile json.RawMessage
	status, err := g.decode(req, &profile)
	switch {
	case err != nil || status >= 500:
		w.WriteHeader(http.StatusServiceUnavailable)
		p.Message = "Profile service unavailable."
	case status == 200:
		if method == http.MethodPut {
			http.Redirect(w, r, "/account/profile", http.StatusSeeOther)
			return
		}
		p.ProfileJSON = string(profile)
	case status == 401:
		w.WriteHeader(status)
		p.Message = "Log in to view your profile."
	case status == 403:
		w.WriteHeader(status)
		p.Message = "Verify your current email address to access your profile."
	case status == 404:
		p.Message = "Your customer profile has not been provisioned."
	default:
		w.WriteHeader(http.StatusBadGateway)
		p.Message = "The profile request failed."
	}
	g.render(w, p)
}

var markup = template.Must(template.New("page").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}}</title></head><body>
<nav aria-label="Account">
<a href="/">Home</a>
{{if not .Operator}}<a href="/self-service/registration/browser">Register</a>{{end}}
<a href="/self-service/login/browser">Log in</a>
{{if .Operator}}<a href="/self-service/login/browser?aal=aal2">Verify second factor</a>
<a href="/account/operator-profiles">Read profiles</a>
{{else}}<a href="/self-service/verification/browser">Verify email</a>
<a href="/self-service/recovery/browser">Recover account</a>{{end}}
<a href="/self-service/settings/browser">Account settings</a>
{{if .Profile}}<a href="/account/profile">Current profile</a>
<a href="/account/preferences">Preferences</a>
<a href="/account/consent">Privacy choices</a>{{end}}
<a href="/account/logout">Log out</a>
</nav><main><h1>{{.Title}}</h1>
{{if .Message}}<p role="status">{{.Message}}</p>{{end}}
{{if .Restart}}<p><a href="{{.Restart}}">Start a new flow</a></p>{{end}}
{{if .TOTPSecret}}<p>Authenticator setup key <code id="totp_secret_key">{{.TOTPSecret}}</code></p>{{end}}
{{if .Logout}}<p><a href="{{.Logout}}">Confirm logout</a></p>{{end}}
{{$action := .Action}}
{{if $action}}{{range .Forms}}<form action="{{$action}}" method="post">
{{range .}}{{if eq .Type "hidden"}}<input type="hidden" name="{{.Name}}" value="{{.Value}}">
{{else if eq .Type "submit"}}<button type="submit" name="{{.Name}}" value="{{.Value}}" {{if .Disabled}}disabled{{end}}>{{.Label}}</button>
{{else}}<p><label>{{.Label}} <input type="{{.Type}}" name="{{.Name}}" value="{{.Value}}" {{if .Disabled}}disabled{{end}} {{if .Required}}required{{end}}></label></p>{{end}}{{end}}
</form>{{end}}{{end}}
{{if .ProfileJSON}}<pre>{{.ProfileJSON}}</pre>{{end}}
{{with .Consent}}{{range .Purposes}}<section>
<h2>{{.PurposeName}}</h2>
<p>{{.Policy.Explanation}}</p>
<dl>
<dt>Notice version</dt><dd>{{.Policy.Version}} ({{.Policy.State}})</dd>
<dt>Notice reference</dt><dd>{{.Policy.NoticeReference}}</dd>
<dt>Legal basis</dt><dd>{{.Policy.LawfulBasis}}</dd>
<dt>Retention</dt><dd>{{.Policy.RetentionReference}}</dd>
<dt>Status</dt><dd>{{.Effective.Outcome}}</dd>
<dt>Your latest choice</dt><dd>{{if .LatestDecision}}{{.LatestDecision}}{{else}}None recorded{{end}}</dd>
</dl>
{{$item := .}}{{range .Actions}}<form action="/account/consent" method="post">
<input type="hidden" name="policy_version_id" value="{{$item.Policy.ID}}">
<input type="hidden" name="revision" value="{{$item.Revision}}">
<input type="hidden" name="idempotency_key" value="{{.Key}}">
<button type="submit" name="decision" value="{{.Decision}}">{{.Label}} {{$item.PurposeName}}</button>
</form>{{end}}
</section>{{else}}<p>There are no privacy choices to make.</p>{{end}}
{{if .Next}}<p><a href="/account/consent?cursor={{.Next}}">More choices</a></p>{{end}}
<section><h2>Decision history</h2>
{{if .History}}<table><thead><tr><th scope="col">Received</th><th scope="col">Purpose</th><th scope="col">Notice version</th><th scope="col">Decision</th><th scope="col">Recorded by</th></tr></thead>
<tbody>{{range .History}}<tr><td>{{.ReceivedAt}}</td><td>{{.PurposeName}}</td><td>{{.PolicyVersion}}</td><td>{{.Decision}}</td><td>{{.ActorKind}}</td></tr>
{{end}}</tbody></table>{{else}}<p>No decisions recorded.</p>{{end}}
{{if .OlderHistory}}<p><a href="/account/consent?history_cursor={{.OlderHistory}}">Older decisions</a></p>{{end}}</section>{{end}}
{{if eq .Message "Your customer profile has not been provisioned."}}
<form action="/account/profile" method="post"><button type="submit">Create my profile</button></form>{{end}}
</main></body></html>`))

func (g *gateway) render(w http.ResponseWriter, p page) {
	p.Operator = g.operator
	p.Profile = p.Profile && !g.operator
	_ = markup.Execute(w, p)
}

type consentPolicy struct {
	ID, State, Explanation string
	LawfulBasis            string `json:"lawful_basis"`
	NoticeReference        string `json:"notice_reference"`
	RetentionReference     string `json:"retention_reference"`
	Version                int
}

type consentAction struct {
	Decision, Label, Key string
}

type consentPurpose struct {
	PurposeName        string `json:"purpose_name"`
	Policy             consentPolicy
	Effective          struct{ Outcome string }
	Revision           int
	LatestDecision     string   `json:"latest_decision"`
	AvailableDecisions []string `json:"available_decisions"`
	Actions            []consentAction
}

type consentRecord struct {
	PurposeName   string    `json:"purpose_name"`
	PolicyVersion int       `json:"policy_version"`
	ActorKind     string    `json:"actor_kind"`
	ReceivedAt    time.Time `json:"received_at"`
	Decision      string
}

type consentView struct {
	Purposes []consentPurpose
	History  []consentRecord
	Next     string
	// OlderHistory is the cursor for decisions before this page.
	OlderHistory string
}

var (
	revisionValue = regexp.MustCompile(`^[1-9][0-9]{0,17}$`)
	consentKey    = regexp.MustCompile(`^[A-Za-z0-9_-]{8,128}$`)
)

// apiJSON calls the profile API as the browser's own session. Only that session and the
// headers the API needs to reject cross-site writes are forwarded.
func (g *gateway) apiJSON(r *http.Request, method, path string, body []byte, headers map[string]string, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, g.api.String()+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	g.authorizeAPI(r, req)
	return g.decode(req, out)
}

func (g *gateway) consent(w http.ResponseWriter, r *http.Request) {
	// A no-referrer page submits Origin: null, which the API must refuse.
	w.Header().Set("Referrer-Policy", "strict-origin")
	if r.Method == http.MethodPost {
		g.submitConsent(w, r)
		return
	}
	g.renderConsent(w, r, http.StatusOK, "")
}

func (g *gateway) submitConsent(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != "http://"+r.Host {
		http.Error(w, "Origin rejected.", http.StatusForbidden)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid consent form.", http.StatusBadRequest)
		return
	}
	policy, decision, revision, key := r.PostForm.Get("policy_version_id"), r.PostForm.Get("decision"), r.PostForm.Get("revision"), r.PostForm.Get("idempotency_key")
	if !flowID.MatchString(policy) || (decision != "grant" && decision != "withdraw") || !revisionValue.MatchString(revision) || !consentKey.MatchString(key) {
		http.Error(w, "Invalid consent form.", http.StatusBadRequest)
		return
	}
	body, err := json.Marshal(map[string]string{"policy_version_id": policy, "decision": decision})
	if err != nil {
		http.Error(w, "Invalid consent form.", http.StatusBadRequest)
		return
	}
	headers := map[string]string{"Idempotency-Key": key}
	if decision == "grant" {
		headers["If-Match"] = `"` + revision + `"`
	}
	var receipt json.RawMessage
	status, err := g.apiJSON(r, http.MethodPost, "/v1/customer/consent/decisions", body, headers, &receipt)
	switch {
	case err != nil || status >= 500 || status == 0:
		g.renderConsent(w, r, http.StatusServiceUnavailable, "Privacy choices are unavailable. Try again.")
	case status == 200 || status == 201:
		http.Redirect(w, r, "/account/consent", http.StatusSeeOther)
	case status == 412:
		g.renderConsent(w, r, http.StatusConflict, "Your choices changed elsewhere. Review the current choices and decide again.")
	case status == 409:
		g.renderConsent(w, r, http.StatusConflict, "That choice is no longer available. Review the current choices and try again.")
	case status == 403:
		g.renderConsent(w, r, status, "This choice cannot be recorded for your account.")
	case status == 401:
		g.renderConsent(w, r, status, "")
	default:
		g.renderConsent(w, r, http.StatusBadGateway, "The choice could not be recorded.")
	}
}

func (g *gateway) renderConsent(w http.ResponseWriter, r *http.Request, failure int, message string) {
	p := page{Title: "Privacy choices", Profile: true, Message: message}
	path := "/v1/customer/consent?limit=50"
	if cursor := r.URL.Query().Get("cursor"); flowID.MatchString(cursor) {
		path += "&cursor=" + cursor
	}
	var state struct {
		Items      []consentPurpose
		NextCursor string `json:"next_cursor"`
	}
	var history struct {
		Items      []consentRecord
		NextCursor string `json:"next_cursor"`
	}
	historyPath := "/v1/customer/consent/history?limit=50"
	if cursor := r.URL.Query().Get("history_cursor"); flowID.MatchString(cursor) {
		historyPath += "&cursor=" + cursor
	}
	status, err := g.apiJSON(r, http.MethodGet, path, nil, nil, &state)
	if err == nil && status == http.StatusOK {
		status, err = g.apiJSON(r, http.MethodGet, historyPath, nil, nil, &history)
	}
	switch {
	case err != nil || status >= 500 || status == 0:
		w.WriteHeader(http.StatusServiceUnavailable)
		p.Message = "Privacy choices are unavailable."
	case status == http.StatusOK:
		for i := range state.Items {
			state.Items[i].Actions = consentActions(state.Items[i].AvailableDecisions)
		}
		p.Consent = &consentView{Purposes: state.Items, History: history.Items, Next: state.NextCursor, OlderHistory: history.NextCursor}
		w.WriteHeader(failure)
	case status == http.StatusUnauthorized:
		w.WriteHeader(status)
		p.Message = "Log in to manage your privacy choices."
	case status == http.StatusForbidden:
		w.WriteHeader(status)
		p.Message = "Verify your current email address to manage your privacy choices."
	case status == http.StatusNotFound:
		p.Message = "Create your profile before making privacy choices."
	default:
		w.WriteHeader(http.StatusBadGateway)
		p.Message = "Privacy choices could not be loaded."
	}
	g.render(w, p)
}

// consentActions gives each rendered form its own key, so resubmitting that form is a safe retry.
func consentActions(decisions []string) []consentAction {
	actions := make([]consentAction, 0, len(decisions))
	for _, decision := range decisions {
		label := "Withdraw"
		if decision == "grant" {
			label = "Allow"
		}
		var raw [16]byte
		_, _ = rand.Read(raw[:])
		actions = append(actions, consentAction{Decision: decision, Label: label, Key: hex.EncodeToString(raw[:])})
	}
	return actions
}

var preferenceVersion = regexp.MustCompile(`^[0-9]{1,18}$`)

type preferencesView struct {
	DisplayName       *string `json:"display_name"`
	PreferredLanguage *string `json:"preferred_language"`
	Timezone          *string `json:"timezone"`
	Version           int64   `json:"version"`
}

// preferences shows and saves the customer's own display name, language and timezone. The API decides
// validity and concurrency; the page only relays the version it showed.
func (g *gateway) preferences(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Referrer-Policy", "strict-origin")
	message := ""
	var submitted *preferencesView
	if r.Method == http.MethodPost {
		if len(r.Header.Values("Origin")) != 1 || r.Header.Get("Origin") != "http://"+r.Host {
			http.Error(w, "Origin rejected.", http.StatusForbidden)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		if err := r.ParseForm(); err != nil || !preferenceVersion.MatchString(r.PostForm.Get("version")) {
			http.Error(w, "Invalid preferences form.", http.StatusBadRequest)
			return
		}
		body, _ := json.Marshal(map[string]string{"display_name": r.PostForm.Get("display_name"),
			"preferred_language": r.PostForm.Get("preferred_language"), "timezone": r.PostForm.Get("timezone")})
		headers := map[string]string{}
		if version := r.PostForm.Get("version"); version != "0" {
			headers["If-Match"] = `"` + version + `"`
		}
		status, err := g.apiJSON(r, http.MethodPut, "/v1/customer/profile/preferences", body, headers, &struct{}{})
		switch {
		case err == nil && status == 200:
			http.Redirect(w, r, "/account/preferences", http.StatusSeeOther)
			return
		case status == 412 || status == 428:
			message = "Your preferences changed elsewhere. They are shown as saved now; review them and save again."
		case status == 422:
			message = "Check the values: use a language tag such as de-DE and a time zone such as Europe/Berlin."
			// Keep what the customer typed so they can correct it.
			name, language, zone := r.PostForm.Get("display_name"), r.PostForm.Get("preferred_language"), r.PostForm.Get("timezone")
			submitted = &preferencesView{DisplayName: &name, PreferredLanguage: &language, Timezone: &zone}
		default:
			message = "Your preferences could not be saved."
		}
	}
	var current preferencesView
	status, err := g.apiJSON(r, http.MethodGet, "/v1/customer/profile/preferences", nil, nil, &current)
	p := page{Title: "Your preferences", Profile: true, Message: message}
	switch {
	case err != nil || status >= 500:
		w.WriteHeader(http.StatusServiceUnavailable)
		p.Message = "Profile service unavailable."
	case status == 401:
		w.WriteHeader(status)
		p.Message = "Log in to manage your preferences."
	case status == 403:
		w.WriteHeader(status)
		p.Message = "Verify your current email address to manage your preferences."
	case status == 404:
		p.Message = "Your customer profile has not been provisioned."
	case status == 200:
		value := func(v *string) string {
			if v == nil {
				return ""
			}
			return *v
		}
		shown := current
		if submitted != nil {
			shown.DisplayName, shown.PreferredLanguage, shown.Timezone = submitted.DisplayName, submitted.PreferredLanguage, submitted.Timezone
		}
		p.Action = "/account/preferences"
		p.Forms = [][]field{{
			{Name: "display_name", Type: "text", Value: value(shown.DisplayName), Label: "Display name"},
			{Name: "preferred_language", Type: "text", Value: value(shown.PreferredLanguage), Label: "Preferred language"},
			{Name: "timezone", Type: "text", Value: value(shown.Timezone), Label: "Time zone"},
			{Name: "version", Type: "hidden", Value: strconv.FormatInt(current.Version, 10)},
			{Name: "save", Type: "submit", Value: "save", Label: "Save preferences"},
		}}
	default:
		w.WriteHeader(http.StatusBadGateway)
		p.Message = "The preferences request failed."
	}
	g.render(w, p)
}
