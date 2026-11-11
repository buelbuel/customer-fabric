package identity

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"
)

var ErrOperatorPermission = errors.New("operator MFA or permission required")

// Operator roles. profile_reader gates every operator read; every write needs its own scoped role, so an
// operator holds only the administrative powers assigned to them.
const (
	RoleProfileReader      = "profile_reader"
	RoleGovernanceAdmin    = "governance_admin"
	RoleConsentAdmin       = "consent_admin"
	RoleAudienceManager    = "audience_manager"
	RoleActivationOperator = "activation_operator"
	RoleIdentityReviewer   = "identity_reviewer"
	RoleSourceAdmin        = "source_admin"
	RoleAuditReader        = "audit_reader"
	RoleApplicationAdmin   = "application_admin"
	RolePrivacyAdmin       = "privacy_admin"
)

var knownRoles = []string{RoleProfileReader, RoleGovernanceAdmin, RoleConsentAdmin, RoleAudienceManager,
	RoleActivationOperator, RoleIdentityReviewer, RoleSourceAdmin, RoleAuditReader, RoleApplicationAdmin, RolePrivacyAdmin}

// AllRoles lists every operator role. Development bearer mode grants all of them; it is not production
// authorization.
func AllRoles() []string { return slices.Clone(knownRoles) }

// Operator is an authenticated named operator and the roles assigned to them.
type Operator struct {
	Subject string
	Roles   []string
}

// Has reports whether the operator holds role.
func (o Operator) Has(role string) bool { return slices.Contains(o.Roles, role) }

// OperatorSubject checks the separate operator issuer on every request, so a role revoked through the
// admin API applies to the next request. metadata_public.customer_fabric_roles is set by the operator
// Kratos admin API, outside self-service; unknown role names are ignored, and an operator without any
// known role is refused.
func (k *Kratos) OperatorSubject(ctx context.Context, bearer string, cookie *http.Cookie) (Operator, error) {
	if (bearer == "") == (cookie == nil) || (bearer != "" && !token.MatchString(bearer)) {
		return Operator{}, ErrInvalidSession
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, k.whoami, nil)
	if err != nil {
		return Operator{}, ErrUnavailable
	}
	req.Header.Set("Accept", "application/json")
	if cookie != nil {
		req.AddCookie(cookie)
	} else {
		req.Header.Set("X-Session-Token", bearer)
	}
	response, err := k.client.Do(req)
	if err != nil {
		return Operator{}, ErrUnavailable
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusUnauthorized:
		return Operator{}, ErrInvalidSession
	case http.StatusForbidden:
		return Operator{}, ErrOperatorPermission
	case http.StatusOK:
	default:
		return Operator{}, ErrUnavailable
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
				Roles []string `json:"customer_fabric_roles"`
			} `json:"metadata_public"`
		} `json:"identity"`
	}
	const maxSessionBytes = 64 << 10
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxSessionBytes+1))
	if err != nil || len(raw) > maxSessionBytes || json.Unmarshal(raw, &session) != nil {
		return Operator{}, ErrUnavailable
	}
	id := session.Identity
	if !session.Active || !session.ExpiresAt.After(time.Now()) || id.State != "active" || id.Schema != "operator" ||
		!subjectID.MatchString(id.ID) || id.ID == "00000000-0000-0000-0000-000000000000" {
		return Operator{}, ErrInvalidSession
	}
	var password, totp bool
	for _, method := range session.Methods {
		password = password || method.Method == "password"
		totp = totp || method.Method == "totp"
	}
	operator := Operator{Subject: id.ID, Roles: []string{}}
	for _, role := range id.Metadata.Roles {
		if slices.Contains(knownRoles, role) && !operator.Has(role) {
			operator.Roles = append(operator.Roles, role)
		}
	}
	if session.Assurance != "aal2" || !password || !totp || len(operator.Roles) == 0 {
		return Operator{}, ErrOperatorPermission
	}
	return operator, nil
}
