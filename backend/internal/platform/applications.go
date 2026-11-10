package platform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
)

const operatorApplicationsPath = "/v1/operator/applications"

// applicationScopes are the scopes an application may be registered for (ADR 0012).
var applicationScopes = []string{"openid", "offline_access", "email", "profile.read", "profile.write"}

var clientID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Application is a registered OAuth client as operators see it.
type Application struct {
	ClientID     string    `json:"client_id"`
	Name         string    `json:"name"`
	Type         string    `json:"type"`
	RedirectURIs []string  `json:"redirect_uris"`
	Scopes       []string  `json:"scopes"`
	CreatedAt    time.Time `json:"created_at"`
	// ClientSecret is returned once, when a confidential application is registered.
	ClientSecret string `json:"client_secret,omitempty"`
}

type applicationRequest struct {
	Name         string   `json:"name"`
	Type         string   `json:"type"`
	RedirectURIs []string `json:"redirect_uris"`
	Scopes       []string `json:"scopes"`
}

// validate enforces the registration policy: a name, a public (PKCE) or confidential client, at most ten
// exact redirect URIs that use https or a loopback http address, and known scopes including openid.
func (req applicationRequest) validate() error {
	if strings.TrimSpace(req.Name) == "" || len(req.Name) > 100 {
		return fmt.Errorf("name is required and at most 100 characters")
	}
	if req.Type != "public" && req.Type != "confidential" {
		return fmt.Errorf("type must be public (PKCE) or confidential")
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		return fmt.Errorf("redirect_uris lists between 1 and 10 exact URIs")
	}
	for _, raw := range req.RedirectURIs {
		u, err := url.Parse(raw)
		loopback := u != nil && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1")
		if err != nil || len(raw) > 512 || u.Fragment != "" || u.User != nil || u.Host == "" || !(u.Scheme == "https" || loopback) {
			return fmt.Errorf("each redirect URI must be an absolute https URI, or http on a loopback address, without a fragment")
		}
	}
	if !slices.Contains(req.Scopes, "openid") {
		return fmt.Errorf("scopes must include openid")
	}
	for _, s := range req.Scopes {
		if !slices.Contains(applicationScopes, s) {
			return fmt.Errorf("scopes are limited to %s", strings.Join(applicationScopes, ", "))
		}
	}
	return nil
}

type hydraClient struct {
	ClientID        string    `json:"client_id,omitempty"`
	ClientName      string    `json:"client_name"`
	ClientSecret    string    `json:"client_secret,omitempty"`
	RedirectURIs    []string  `json:"redirect_uris"`
	GrantTypes      []string  `json:"grant_types"`
	ResponseTypes   []string  `json:"response_types"`
	Scope           string    `json:"scope"`
	Audience        []string  `json:"audience"`
	TokenAuthMethod string    `json:"token_endpoint_auth_method"`
	CreatedAt       time.Time `json:"created_at,omitempty"`
}

func applicationFrom(c hydraClient) Application {
	kind := "confidential"
	if c.TokenAuthMethod == "none" {
		kind = "public"
	}
	return Application{ClientID: c.ClientID, Name: c.ClientName, Type: kind, RedirectURIs: c.RedirectURIs,
		Scopes: strings.Fields(c.Scope), CreatedAt: c.CreatedAt, ClientSecret: c.ClientSecret}
}

// hydra calls Hydra's admin API.
func (a *API) hydra(r *http.Request, method, path string, body, out any) (int, error) {
	var payload io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		payload = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(r.Context(), method, strings.TrimSuffix(a.Config.HydraAdminURL, "/")+path, payload)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer res.Body.Close()
	if out != nil && res.StatusCode < 300 {
		err = json.NewDecoder(io.LimitReader(res.Body, 1<<20)).Decode(out)
	}
	return res.StatusCode, err
}

// serveApplications registers, lists and deletes OAuth applications for operators with application_admin.
func (a *API) serveApplications(w http.ResponseWriter, r *http.Request) {
	if a.Config.HydraAdminURL == "" {
		writeProblem(w, problem(404, "Not found", "OAuth applications are not configured."))
		return
	}
	id := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, operatorApplicationsPath), "/")
	methods := []string{http.MethodGet, http.MethodPost}
	if id != "" {
		methods = []string{http.MethodDelete}
		if !clientID.MatchString(id) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	// Registered clients and their redirect URIs are administrative data, so reading them needs the role too.
	if !allow(w, r, methods...) || !a.authorizeConsent(w, r, identity.RoleApplicationAdmin) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	unavailable := func() {
		writeProblem(w, problem(503, "Authorization service unavailable", "The OAuth provider could not be reached."))
	}
	switch r.Method {
	case http.MethodGet:
		var clients []hydraClient
		if status, err := a.hydra(r, http.MethodGet, "/admin/clients?page_size=500", nil, &clients); err != nil || status != http.StatusOK {
			unavailable()
			return
		}
		items := make([]Application, 0, len(clients))
		for _, c := range clients {
			c.ClientSecret = ""
			items = append(items, applicationFrom(c))
		}
		writeJSON(w, 200, map[string]any{"items": items})
	case http.MethodDelete:
		status, err := a.hydra(r, http.MethodDelete, "/admin/clients/"+url.PathEscape(id), nil, nil)
		switch {
		case err != nil || status >= 500:
			unavailable()
		case status == http.StatusNotFound:
			writeProblem(w, problem(404, "Not found", "Unknown application."))
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		var req applicationRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if err := req.validate(); err != nil {
			writeProblem(w, problem(422, "Invalid application", err.Error()))
			return
		}
		client := hydraClient{ClientName: strings.TrimSpace(req.Name), RedirectURIs: req.RedirectURIs, GrantTypes: []string{"authorization_code"},
			ResponseTypes: []string{"code"}, Scope: strings.Join(req.Scopes, " "), Audience: []string{"customer-fabric-api"}, TokenAuthMethod: "client_secret_basic"}
		if slices.Contains(req.Scopes, "offline_access") {
			client.GrantTypes = append(client.GrantTypes, "refresh_token")
		}
		if req.Type == "public" {
			client.TokenAuthMethod = "none"
		}
		var created hydraClient
		if status, err := a.hydra(r, http.MethodPost, "/admin/clients", client, &created); err != nil || status != http.StatusCreated {
			unavailable()
			return
		}
		writeJSON(w, 201, applicationFrom(created))
	}
}
