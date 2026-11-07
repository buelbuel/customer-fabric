package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"
)

var ErrOperatorPermission = errors.New("operator MFA or permission required")

// OperatorSubject checks the separate operator issuer on every request.
// metadata_public is set by the operator Kratos admin API, outside self-service.
func (k *Kratos) OperatorSubject(ctx context.Context, bearer string, cookie *http.Cookie) (string, error) {
	if (bearer == "") == (cookie == nil) || (bearer != "" && !token.MatchString(bearer)) {
		return "", ErrInvalidSession
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.whoami, nil)
	if err != nil {
		return "", ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	} else {
		req.Header.Set("X-Session-Token", bearer)
	}
	response, err := k.client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return "", ErrInvalidSession
	case http.StatusForbidden:
		return "", ErrOperatorPermission
	case http.StatusOK:
	default:
		return "", ErrUnavailable
	}
	var session struct {
		Active    bool      `json:"active"`
		ExpiresAt time.Time `json:"expires_at"`
		Assurance string    `json:"authenticator_assurance_level"`
		Methods   []struct {
			Method string `json:"method"`
		} `json:"authentication_methods"`
		Identity struct {
			ID       string `json:"id"`
			Schema   string `json:"schema_id"`
			State    string `json:"state"`
			Metadata struct {
				Role string `json:"customer_fabric_role"`
			} `json:"metadata_public"`
		} `json:"identity"`
	}
	const maxSessionBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSessionBytes+1))
	if err != nil || len(raw) > maxSessionBytes || json.Unmarshal(raw, &session) != nil {
		return "", ErrUnavailable
	}
	id := session.Identity
	if !session.Active || !session.ExpiresAt.After(time.Now()) || id.State != "active" || id.Schema != "operator" ||
		!subjectID.MatchString(id.ID) || id.ID == "00000000-0000-0000-0000-000000000000" {
		return "", ErrInvalidSession
	}
	var password, totp bool
	for _, method := range session.Methods {
		password = password || method.Method == "password"
		totp = totp || method.Method == "totp"
	}
	if session.Assurance != "aal2" || !password || !totp || id.Metadata.Role != "profile_reader" {
		return "", ErrOperatorPermission
	}
	return id.ID, nil
}
