package platform

import (
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	sourceConsentPath            = "/v1/source-consent-decisions"
	operatorConsentAuthorityPath = "/v1/operator/consent-authorities"
)

// serveSourceConsent records a consent decision the authenticated source reports about one of its own
// records (ADR 0015). The source comes from the credential, never from the body.
func (a *API) serveSourceConsent(w http.ResponseWriter, r *http.Request, source string) {
	if !allow(w, r, http.MethodPost) {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	key := r.Header.Values("Idempotency-Key")
	if len(key) != 1 || !keyPattern.MatchString(key[0]) {
		writeProblem(w, problem(400, "Invalid idempotency key", "Supply one Idempotency-Key of 16 to 128 letters, digits, hyphens or underscores."))
		return
	}
	var body SourceConsentDecision
	if !decodeJSON(w, r, &body) {
		return
	}
	receipt, err := a.Store.AppendSourceConsent(r.Context(), source, key[0], body)
	if err != nil {
		fail(w, err)
		return
	}
	status := http.StatusOK
	if receipt.Created {
		status = http.StatusCreated
	}
	writeJSON(w, status, receipt)
}

// serveConsentAuthorities lists and approves consent authorities, and renews, suspends or revokes one,
// manages its receipt keys and lists its audit trail (ADR 0015, ADR 0017). Reading needs profile_reader;
// every change decides what a source may attest and needs privacy_admin.
func (a *API) serveConsentAuthorities(w http.ResponseWriter, r *http.Request) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.URL.Path == operatorConsentAuthorityPath {
		if !allow(w, r, http.MethodGet, http.MethodPost) {
			return
		}
		if r.Method == http.MethodGet {
			if !a.authorizeConsent(w, r, identity.RoleProfileReader) {
				return
			}
			items, err := a.Store.ConsentAuthorities(r.Context())
			if err != nil {
				fail(w, err)
				return
			}
			writeJSON(w, 200, map[string]any{"items": items})
			return
		}
		operator, ok := a.authorizeRole(w, r, identity.RolePrivacyAdmin)
		if !ok {
			return
		}
		var body ConsentAuthorityRequest
		if !decodeJSON(w, r, &body) {
			return
		}
		actor := operator.Subject
		if actor == "" {
			actor = "development-bearer"
		}
		result, err := a.Store.ApproveConsentAuthority(r.Context(), body, actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 201, result)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, operatorConsentAuthorityPath+"/"), "/")
	if len(parts) < 2 || len(parts) > 4 || !profiles.ID.MatchString(parts[0]) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	id := parts[0]
	read := func(load func() (any, error)) {
		if !allow(w, r, http.MethodGet) || !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		items, err := load()
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
	}
	// write authorizes a change to what a source may attest, which needs privacy_admin, and decodes the
	// request into body unless it is nil.
	write := func(status int, body any, change func(actor string) (any, error)) {
		actor, ok := a.privacyAdmin(w, r)
		if !ok || body != nil && !decodeJSON(w, r, body) {
			return
		}
		result, err := change(actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, status, result)
	}
	switch {
	case len(parts) == 2 && parts[1] == "audit":
		read(func() (any, error) { return a.Store.AuthorityAudit(r.Context(), id) })
	case len(parts) == 2 && parts[1] == "keys" && r.Method == http.MethodGet:
		read(func() (any, error) { return a.Store.AuthorityKeys(r.Context(), id) })
	case len(parts) == 2 && parts[1] == "keys":
		var body AuthorityKeyRequest
		write(201, &body, func(actor string) (any, error) { return a.Store.AddAuthorityKey(r.Context(), id, body, actor) })
	case len(parts) == 4 && parts[1] == "keys" && (parts[3] == "retire" || parts[3] == "compromise"):
		state := map[string]string{"retire": "retired", "compromise": "compromised"}[parts[3]]
		var body struct {
			Reference string `json:"reference"`
		}
		write(200, &body, func(actor string) (any, error) {
			return a.Store.SetAuthorityKeyState(r.Context(), id, parts[2], state, body.Reference, actor)
		})
	case len(parts) == 2 && parts[1] == "revoke":
		var body struct {
			Reference string `json:"reference"`
		}
		write(200, &body, func(actor string) (any, error) {
			return a.Store.RevokeConsentAuthority(r.Context(), id, body.Reference, actor)
		})
	case len(parts) == 2 && parts[1] == "suspend":
		var body struct {
			Reference string `json:"reference"`
		}
		write(200, &body, func(actor string) (any, error) {
			return a.Store.SuspendConsentAuthority(r.Context(), id, body.Reference, actor)
		})
	case len(parts) == 2 && parts[1] == "renew":
		var body AuthorityReview
		write(200, &body, func(actor string) (any, error) { return a.Store.RenewConsentAuthority(r.Context(), id, body, actor) })
	default:
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
	}
}
