package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/customer-fabric/customer-fabric/backend/internal/identity"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

type Config struct {
	Mode                    string
	Tokens                  map[string]string
	OperatorToken           string
	KratosPublicURL         string
	OperatorKratosPublicURL string
	OperatorBrowserOrigin   string
	CustomerBrowserOrigin   string
	LinkingIssuers          map[string]string
	// WebhookHosts are the host:port pairs test webhook destinations may target. Empty disables activation
	// destinations and the dispatcher.
	WebhookHosts []string
}

var developmentBrowserOrigin = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]{1,5}$`)

func (c Config) Validate() error {
	if c.OperatorBrowserOrigin != "" && (c.OperatorKratosPublicURL == "" || c.OperatorBrowserOrigin == c.CustomerBrowserOrigin ||
		!developmentBrowserOrigin.MatchString(c.OperatorBrowserOrigin)) {
		return fmt.Errorf("invalid development operator browser origin")
	}
	if c.CustomerBrowserOrigin != "" && (c.KratosPublicURL == "" || !developmentBrowserOrigin.MatchString(c.CustomerBrowserOrigin)) {
		return fmt.Errorf("invalid development customer browser origin")
	}
	if c.Mode != "development" {
		return fmt.Errorf("only explicit development mode is supported")
	}
	if len(c.Tokens) == 0 || len(c.Tokens) > 20 {
		return fmt.Errorf("configure 1 to 20 source credentials")
	}
	seen := map[string]bool{}
	for source, token := range c.Tokens {
		if !profiles.Name.MatchString(source) || !tokenPattern.MatchString(token) ||
			seen[token] || ((c.KratosPublicURL != "" || c.OperatorKratosPublicURL != "") && strings.HasPrefix(token, "ory_st_")) {
			return fmt.Errorf("invalid source credential configuration")
		}
		seen[token] = true
	}
	if c.OperatorToken != "" && (!tokenPattern.MatchString(c.OperatorToken) ||
		seen[c.OperatorToken] || strings.HasPrefix(c.OperatorToken, "ory_st_")) {
		return fmt.Errorf("configure a distinct operator credential")
	}
	if c.OperatorKratosPublicURL != "" {
		customer, customerErr := url.Parse(c.KratosPublicURL)
		operator, operatorErr := url.Parse(c.OperatorKratosPublicURL)
		if c.OperatorToken != "" || c.KratosPublicURL == "" || customerErr != nil || operatorErr != nil ||
			strings.EqualFold(customer.Host, operator.Host) {
			return fmt.Errorf("operator identity mode requires a distinct issuer and disables the operator bearer credential")
		}
	}
	if c.KratosPublicURL != "" && c.OperatorToken == "" && c.OperatorKratosPublicURL == "" {
		return fmt.Errorf("customer identity mode requires a separate operator credential")
	}
	if c.OperatorToken != "" {
		seen[c.OperatorToken] = true
	}
	if len(c.LinkingIssuers) > 0 && c.KratosPublicURL == "" {
		return fmt.Errorf("linking requires customer identity mode")
	}
	for source, token := range c.LinkingIssuers {
		if _, ok := c.Tokens[source]; !ok || !tokenPattern.MatchString(token) ||
			seen[token] || strings.HasPrefix(token, "ory_st_") {
			return fmt.Errorf("configure distinct linking issuer credentials for known sources")
		}
		seen[token] = true
	}
	if len(c.WebhookHosts) > 10 {
		return fmt.Errorf("configure at most 10 webhook hosts")
	}
	for _, host := range c.WebhookHosts {
		_, port, _ := strings.Cut(host, ":")
		if n, err := strconv.Atoi(port); !webhookHost.MatchString(host) || err != nil || n < 1 || n > 65535 {
			return fmt.Errorf("webhook hosts are host:port pairs")
		}
	}
	return nil
}

var webhookHost = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,252}[a-z0-9])?:[0-9]{1,5}$`)

type API struct {
	Store          Store
	Config         Config
	kratos         *identity.Kratos
	operatorKratos *identity.Kratos
	mu             sync.Mutex
	window         time.Time
	requests       int
}

func NewAPI(s Store, c Config) (http.Handler, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	a := &API{Store: s, Config: c}
	if c.KratosPublicURL != "" {
		var err error
		a.kratos, err = identity.NewKratos(c.KratosPublicURL)
		if err != nil {
			return nil, err
		}
	}
	if c.OperatorKratosPublicURL != "" {
		var err error
		a.operatorKratos, err = identity.NewKratos(c.OperatorKratosPublicURL)
		if err != nil {
			return nil, err
		}
	}
	return http.HandlerFunc(a.serve), nil
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeProblem(w http.ResponseWriter, p *Problem) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(p.Status)
	_ = json.NewEncoder(w).Encode(p)
}
func fail(w http.ResponseWriter, err error) {
	var p *Problem
	if errors.As(err, &p) {
		writeProblem(w, p)
		return
	}
	writeProblem(w, problem(500, "Internal server error", "The operation could not be completed."))
}

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)
var versionPattern = regexp.MustCompile(`^"[1-9][0-9]*"$`)
var keyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)

func (a *API) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		writeJSON(w, 200, map[string]string{"status": "alive"})
		return
	}
	// A bounded global limiter also covers invalid credentials, without storing attacker-controlled keys.
	a.mu.Lock()
	if time.Since(a.window) >= time.Minute {
		a.window = time.Now()
		a.requests = 0
	}
	a.requests++
	limited := a.requests > 600
	a.mu.Unlock()
	if limited {
		w.Header().Set("Retry-After", "60")
		writeProblem(w, problem(429, "Too many requests", "Development API request limit exceeded."))
		return
	}
	for _, name := range []string{"Authorization", "X-Session-Token", "Idempotency-Key", "If-Match"} {
		if len(r.Header.Values(name)) > 1 {
			writeProblem(w, problem(400, "Ambiguous header", "Authentication and mutation headers must occur once."))
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
	if r.URL.Path == associationPath || strings.HasPrefix(r.URL.Path, associationPath+"/") {
		a.serveAssociations(w, r)
		return
	}
	if r.URL.Path == grantPath || strings.HasPrefix(r.URL.Path, grantPath+"/") {
		a.serveLinkingIssuer(w, r)
		return
	}
	if r.URL.Path == customerConsentPath || strings.HasPrefix(r.URL.Path, customerConsentPath+"/") {
		a.serveCustomerConsent(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/v1/customer/") {
		a.serveCustomer(w, r)
		return
	}
	if len(r.Header.Values("X-Session-Token")) != 0 {
		writeProblem(w, problem(401, "Unauthorized", "Customer sessions are scoped to customer resources."))
		return
	}
	if r.URL.Path == operatorProfilesPath || strings.HasPrefix(r.URL.Path, operatorProfilesPath+"/") {
		a.serveOperator(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, operatorCustomersPath) {
		a.serveCustomerView(w, r)
		return
	}
	if r.URL.Path == operatorAttributesPath || strings.HasPrefix(r.URL.Path, operatorAttributesPath+"/") {
		a.serveAttributes(w, r)
		return
	}
	if r.URL.Path == operatorDestinationsPath || strings.HasPrefix(r.URL.Path, operatorDestinationsPath+"/") {
		a.serveDestinations(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, operatorActivationsPath) {
		a.serveActivation(w, r)
		return
	}
	if r.URL.Path == operatorSegmentsPath || strings.HasPrefix(r.URL.Path, operatorSegmentsPath+"/") {
		a.serveSegments(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, consentPrefix) {
		a.serveConsent(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, operatorSourcesPath) {
		a.serveSourceEpoch(w, r)
		return
	}
	source := ""
	provided := sha256.Sum256([]byte(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")))
	if strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		for s, t := range a.Config.Tokens {
			expected := sha256.Sum256([]byte(t))
			if subtle.ConstantTimeCompare(provided[:], expected[:]) == 1 {
				source = s
			}
		}
	}
	if source == "" {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A configured development source credential is required."))
		return
	}
	if r.URL.Path == sourceEventsPath {
		a.serveSourceEvents(w, r, source)
		return
	}
	id := ""
	collection := r.URL.Path == "/v1/profiles"
	if !collection {
		id = strings.TrimPrefix(r.URL.Path, "/v1/profiles/")
		if !strings.HasPrefix(r.URL.Path, "/v1/profiles/") || !profiles.ID.MatchString(id) {
			writeProblem(w, problem(404, "Not found", "Unknown API resource."))
			return
		}
	}
	if r.Method == http.MethodGet {
		if a.kratos != nil {
			writeProblem(w, problem(403, "Forbidden", "Integration credentials cannot read profiles."))
			return
		}
		a.serveProfileRead(w, r, id, collection)
		return
	}
	if (collection && r.Method != http.MethodPost) || (!collection && r.Method != http.MethodPatch) {
		if collection {
			w.Header().Set("Allow", "GET, POST")
		} else {
			w.Header().Set("Allow", "GET, PATCH")
		}
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "Mutations do not accept query parameters."))
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !keyPattern.MatchString(key) {
		writeProblem(w, problem(400, "Invalid idempotency key", "Use 16–128 ASCII letters, digits, underscores or hyphens."))
		return
	}
	version := int64(0)
	if !collection {
		etag := r.Header.Get("If-Match")
		if etag == "" {
			writeProblem(w, problem(428, "Precondition required", "Supply the profile ETag in If-Match."))
			return
		}
		if !versionPattern.MatchString(etag) {
			writeProblem(w, problem(400, "Invalid precondition", "Use a quoted positive version."))
			return
		}
		version, err = strconv.ParseInt(etag[1:len(etag)-1], 10, 64)
		if err != nil || version < 1 {
			writeProblem(w, problem(400, "Invalid precondition", "Use a quoted positive version."))
			return
		}
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeProblem(w, problem(413, "Payload too large", "Request body must not exceed 64 KiB."))
		return
	}
	if !utf8.Valid(raw) || !json.Valid(raw) || duplicateKeys(raw) {
		writeProblem(w, problem(400, "Invalid JSON", "Supply one JSON object without duplicate keys."))
		return
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) == nil {
		for name, v := range fields {
			if name != "set" && name != "remove" && name != "identifiers" {
				writeProblem(w, problem(400, "Invalid payload", "Unknown fields or incorrect field types."))
				return
			}
			if collection && name == "remove" {
				writeProblem(w, problem(422, "Invalid mutation", "remove is unsupported during creation"))
				return
			}
			if name == "identifiers" {
				var identifiers []map[string]json.RawMessage
				if json.Unmarshal(v, &identifiers) == nil {
					for _, identifier := range identifiers {
						for field := range identifier {
							if field != "namespace" && field != "value" {
								writeProblem(w, problem(400, "Invalid payload", "Identifier fields must be namespace and value."))
								return
							}
						}
					}
				}
			}
			if bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
				writeProblem(w, problem(422, "Invalid mutation", "Mutation fields cannot be null."))
				return
			}
		}
	}
	var m profiles.Mutation
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err = dec.Decode(&m); err != nil {
		writeProblem(w, problem(400, "Invalid payload", "Unknown fields or incorrect field types."))
		return
	}
	if err = m.Validate(collection); err != nil {
		writeProblem(w, problem(422, "Invalid mutation", err.Error()))
		return
	}
	canonical, _ := json.Marshal(m)
	digest := sha256.Sum256(append([]byte(r.Method+"\n"+r.URL.Path+"\n"+strconv.FormatInt(version, 10)+"\n"), canonical...))
	if a.kratos != nil && !collection {
		owned, err := db.New(a.Store.Pool).SourceOwnsProfile(ctx, db.SourceOwnsProfileParams{
			ProfileID: id,
			Source:    source,
		})
		if err != nil {
			fail(w, err)
			return
		}
		if !owned {
			writeProblem(w, problem(403, "Forbidden", "This profile is outside the integration source's scope."))
			return
		}
	}
	status, body, err := a.Store.Mutate(ctx, source, key, hex.EncodeToString(digest[:]), id, version, m)
	if err != nil {
		fail(w, err)
		return
	}
	if status >= 400 {
		w.Header().Set("Content-Type", "application/problem+json")
	} else {
		w.Header().Set("Content-Type", "application/json")
		var receipt profiles.Receipt
		_ = json.Unmarshal(body, &receipt)
		w.Header().Set("ETag", fmt.Sprintf(`"%d"`, receipt.Version))
		if status == 201 {
			w.Header().Set("Location", "/v1/profiles/"+receipt.ID)
		}
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// Reject ambiguous JSON before normal decoding can silently overwrite duplicate keys.
func duplicateKeys(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func() bool
	walk = func() bool {
		t, e := d.Token()
		if e != nil {
			return true
		}
		delim, ok := t.(json.Delim)
		if !ok {
			return false
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for d.More() {
				k, e := d.Token()
				if e != nil {
					return true
				}
				s, ok := k.(string)
				if !ok || seen[s] {
					return true
				}
				seen[s] = true
				if walk() {
					return true
				}
			}
		case '[':
			for d.More() {
				if walk() {
					return true
				}
			}
		default:
			return true
		}
		_, e = d.Token()
		return e != nil
	}
	return walk()
}
