package browser

import (
	"encoding/json"
	"net/http"
)

func (g *gateway) operatorProfiles(w http.ResponseWriter, r *http.Request) {
	p := page{Title: "Read operator profiles"}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, g.api.String()+"/v1/operator/profiles?limit=20", nil)
	if err != nil {
		http.Error(w, "Profile service unavailable.", http.StatusServiceUnavailable)
		return
	}
	req.Header.Set("Origin", "http://"+r.Host)
	for _, cookie := range r.CookiesNamed(g.sessionCookie(r.Host)) {
		req.AddCookie(cookie)
	}
	var profiles json.RawMessage
	status, err := g.decode(req, &profiles)
	switch {
	case err != nil || status >= 500:
		w.WriteHeader(http.StatusServiceUnavailable)
		p.Message = "Profile service unavailable."
	case status == http.StatusOK:
		p.ProfileJSON = string(profiles)
	case status == http.StatusUnauthorized:
		w.WriteHeader(status)
		p.Message = "Sign in with your operator account."
	case status == http.StatusForbidden:
		w.WriteHeader(status)
		p.Message = "Verify your second factor. Profile read permission is required."
	default:
		w.WriteHeader(http.StatusBadGateway)
		p.Message = "The profile request failed."
	}
	g.render(w, p)
}
