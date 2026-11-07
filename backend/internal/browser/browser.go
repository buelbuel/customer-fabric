// Package browser renders Kratos browser flows for the local reference UI.
package browser

import (
	"context"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
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
	const max = 128 << 10
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
	req.Header.Set("Origin", "http://"+r.Host)
	for _, cookie := range r.CookiesNamed(g.sessionCookie(r.Host)) {
		req.AddCookie(cookie)
	}
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
{{if .Profile}}<a href="/account/profile">Current profile</a>{{end}}
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
{{if eq .Message "Your customer profile has not been provisioned."}}
<form action="/account/profile" method="post"><button type="submit">Create my profile</button></form>{{end}}
</main></body></html>`))

func (g *gateway) render(w http.ResponseWriter, p page) {
	p.Operator = g.operator
	p.Profile = p.Profile && !g.operator
	_ = markup.Execute(w, p)
}
