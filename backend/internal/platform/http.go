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
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

type Config struct {
	Mode   string
	Tokens map[string]string
}

func (c Config) Validate() error {
	if c.Mode != "development" {
		return fmt.Errorf("M0 only supports explicit development mode; production authentication is not implemented")
	}
	if len(c.Tokens) == 0 || len(c.Tokens) > 20 {
		return fmt.Errorf("configure 1 to 20 source credentials")
	}
	seen := map[string]bool{}
	for source, token := range c.Tokens {
		if !profiles.Name.MatchString(source) || len(token) < 32 || len(token) > 256 || strings.TrimSpace(token) != token || seen[token] {
			return fmt.Errorf("invalid source credential configuration")
		}
		seen[token] = true
	}
	return nil
}

type API struct {
	Store    Store
	Config   Config
	mu       sync.Mutex
	window   time.Time
	requests int
}

func NewAPI(s Store, c Config) (http.Handler, error) {
	if e := c.Validate(); e != nil {
		return nil, e
	}
	a := &API{Store: s, Config: c}
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
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	r = r.WithContext(ctx)
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
		if collection {
			q := r.URL.Query()
			for k := range q {
				if k != "cursor" && k != "limit" {
					writeProblem(w, problem(400, "Invalid query", "Only cursor and limit are supported."))
					return
				}
			}
			cursor := q.Get("cursor")
			limit := 50
			var err error
			if q.Get("limit") != "" {
				limit, err = strconv.Atoi(q.Get("limit"))
			}
			if err != nil || limit < 1 || limit > 100 || (cursor != "" && !profiles.ID.MatchString(cursor)) {
				writeProblem(w, problem(400, "Invalid query", "Use a UUID cursor and limit between 1 and 100."))
				return
			}
			p, e := a.Store.List(ctx, cursor, limit)
			if e != nil {
				fail(w, e)
				return
			}
			writeJSON(w, 200, p)
		} else {
			p, e := a.Store.Get(ctx, id)
			if e != nil {
				fail(w, e)
				return
			}
			w.Header().Set("ETag", fmt.Sprintf(`"%d"`, p.Version))
			writeJSON(w, 200, p)
		}
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
		if len(etag) < 3 || etag[0] != '"' || etag[len(etag)-1] != '"' {
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
	if !json.Valid(raw) || duplicateKeys(raw) {
		writeProblem(w, problem(400, "Invalid JSON", "Supply one JSON object without duplicate keys."))
		return
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
