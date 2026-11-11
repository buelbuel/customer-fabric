package platform

import (
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

const (
	sourceConsentPath            = "/v1/source-consent-decisions"
	sourceVerificationsPath      = "/v1/source-consent-verifications"
	operatorConsentAuthorityPath = "/v1/operator/consent-authorities"
	operatorEvidencePath         = "/v1/operator/consent-evidence/"
)

// sourceConsentLimited counts a source's consent request and refuses it once the source is over its rate.
func (a *API) sourceConsentLimited(w http.ResponseWriter, r *http.Request, source string) bool {
	limited, err := a.Store.CountSourceConsentRequest(r.Context(), source)
	if err != nil {
		fail(w, err)
		return true
	}
	if limited {
		w.Header().Set("Retry-After", "60")
		writeProblem(w, problem(429, "Too many requests", "This source's consent request limit is exceeded; retry after a minute."))
	}
	return limited
}

// serveSourceVerifications lists the verifications the authenticated source must answer, and takes the
// proof document for one (ADR 0017). The document is checked and discarded, never stored.
func (a *API) serveSourceVerifications(w http.ResponseWriter, r *http.Request, source string) {
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.URL.Path == sourceVerificationsPath {
		if !allow(w, r, http.MethodGet) {
			return
		}
		items, err := a.Store.OpenVerifications(r.Context(), source)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	id, ok := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, sourceVerificationsPath+"/"), "/proof")
	if !ok || !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if !allow(w, r, http.MethodPost) {
		return
	}
	if media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || media != "application/json" {
		writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
		return
	}
	// The digest is over the exact bytes, so the body is read as sent, not re-encoded.
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, proofDocumentLimit))
	if err != nil {
		writeProblem(w, problem(413, "Payload too large", "A proof document must not exceed 64 KiB."))
		return
	}
	result, err := a.Store.SubmitProof(r.Context(), source, id, raw)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 200, result)
}

// serveEvidenceVerifications requests, lists and reviews verifications of one source grant. Reading needs
// profile_reader; requesting and reviewing need privacy_admin.
func (a *API) serveEvidenceVerifications(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, operatorEvidencePath), "/")
	if len(parts) == 4 && profiles.ID.MatchString(parts[0]) && parts[1] == "verifications" && profiles.ID.MatchString(parts[2]) && parts[3] == "review" {
		if r.URL.RawQuery != "" {
			writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
			return
		}
		actor, ok := a.privacyAdmin(w, r)
		if !ok {
			return
		}
		var body struct {
			Decision  string `json:"decision"`
			Reference string `json:"reference"`
		}
		if !decodeJSON(w, r, &body) {
			return
		}
		result, err := a.Store.ReviewVerification(r.Context(), parts[0], parts[2], body.Decision, body.Reference, actor)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, result)
		return
	}
	if len(parts) != 2 || !profiles.ID.MatchString(parts[0]) || parts[1] != "verifications" {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	id := parts[0]
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	if r.Method == http.MethodGet {
		if !a.authorizeConsent(w, r, identity.RoleProfileReader) {
			return
		}
		items, err := a.Store.EvidenceVerifications(r.Context(), id)
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"items": items})
		return
	}
	actor, ok := a.privacyAdmin(w, r)
	if !ok {
		return
	}
	var body struct {
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	result, err := a.Store.RequestVerification(r.Context(), id, body.Reason, actor)
	if err != nil {
		fail(w, err)
		return
	}
	writeJSON(w, 201, result)
}

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
	// Grants count against the source's rate limit; withdrawals, denials and objections never wait.
	if body.Decision == "grant" && a.sourceConsentLimited(w, r, source) {
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
	case len(parts) == 2 && parts[1] == "audits":
		var body struct {
			Sample int `json:"sample"`
		}
		write(201, &body, func(actor string) (any, error) {
			items, err := a.Store.AuditAuthority(r.Context(), id, body.Sample, actor)
			return map[string]any{"items": items}, err
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
