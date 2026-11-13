package platform

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

const (
	grantPath       = "/v1/linking/grants"
	associationPath = "/v1/customer/associations"
)

func (a *API) serveLinkingIssuer(w http.ResponseWriter, r *http.Request) {
	if len(a.Config.LinkingIssuers) == 0 {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	provided := sha256.Sum256([]byte(token))
	source := ""
	for s, t := range a.Config.LinkingIssuers {
		expected := sha256.Sum256([]byte(t))
		if subtle.ConstantTimeCompare(provided[:], expected[:]) == 1 {
			source = s
		}
	}
	if !bearer || source == "" || len(r.Header.Values("X-Session-Token")) != 0 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeProblem(w, problem(401, "Unauthorized", "A dedicated linking issuer credential is required."))
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	ctx := r.Context()
	if r.URL.Path == grantPath {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
			return
		}
		var claim struct {
			Namespace       string `json:"namespace"`
			Record          string `json:"record"`
			CustomerSubject string `json:"customer_subject"`
			ProofMethod     string `json:"proof_method"`
			ProofReference  string `json:"proof_reference"`
		}
		if !decodeLinkingJSON(w, r, &claim, "namespace", "record", "customer_subject", "proof_method", "proof_reference") {
			return
		}
		if !profiles.Name.MatchString(claim.Namespace) || len(claim.Record) == 0 || len(claim.Record) > 256 ||
			strings.ContainsFunc(claim.Record, unicode.IsControl) || !profiles.ID.MatchString(claim.CustomerSubject) ||
			claim.ProofMethod != "synthetic_challenge" || !profiles.ID.MatchString(claim.ProofReference) {
			writeProblem(w, problem(422, "Invalid proof", "A supported independent proof and a valid target are required."))
			return
		}
		result, err := a.Store.issueLinkGrant(ctx, linkRecord{source, claim.Namespace, claim.Record}, claim.CustomerSubject, claim.ProofReference)
		if err != nil {
			fail(w, err)
			return
		}
		w.Header().Set("Location", grantPath+"/"+result.ID)
		writeJSON(w, 201, result)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, grantPath+"/")
	if !profiles.ID.MatchString(id) {
		writeProblem(w, problem(404, "Not found", "Unknown API resource."))
		return
	}
	if r.Method != http.MethodDelete {
		w.Header().Set("Allow", "DELETE")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		return
	}
	if !emptyLinkingBody(w, r) {
		return
	}
	if err := a.Store.revokeLinkGrant(ctx, source, id); err != nil {
		fail(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) serveAssociations(w http.ResponseWriter, r *http.Request) {
	session, ok := a.authenticateCustomer(w, r)
	if !ok {
		return
	}
	ctx, subject := r.Context(), session.Subject
	collection := r.URL.Path == associationPath
	if (!collection || r.Method != http.MethodGet) && r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "This resource does not accept query parameters."))
		return
	}
	q := db.New(a.Store.Pool)
	if collection {
		switch r.Method {
		case http.MethodGet:
			if emptyLinkingBody(w, r) {
				listAssociations(w, r, q, subject)
			}
		case http.MethodPost:
			a.redeemAssociation(w, r, subject)
		default:
			w.Header().Set("Allow", "GET, POST")
			writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
		}
		return
	}
	id := strings.TrimPrefix(r.URL.Path, associationPath+"/")
	if !profiles.ID.MatchString(id) {
		writeProblem(w, associationNotFound())
		return
	}
	if !emptyLinkingBody(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		row, err := q.GetActiveVerifiedAssociation(ctx, db.GetActiveVerifiedAssociationParams{ID: id, CustomerSubject: subject})
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(w, associationNotFound())
			return
		}
		if err != nil {
			fail(w, err)
			return
		}
		writeJSON(w, 200, verifiedAssociation(row))
	case http.MethodDelete:
		if err := a.Store.unlinkAssociation(ctx, subject, id); err != nil {
			fail(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, DELETE")
		writeProblem(w, problem(405, "Method not allowed", "Unsupported method."))
	}
}

func (a *API) redeemAssociation(w http.ResponseWriter, r *http.Request, subject string) {
	var input struct {
		Grant string `json:"grant"`
	}
	if !decodeLinkingJSON(w, r, &input, "grant") {
		return
	}
	if len(input.Grant) != base64.RawURLEncoding.EncodedLen(32) {
		writeProblem(w, unusableGrant())
		return
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(input.Grant)
	if err != nil || len(raw) != 32 {
		writeProblem(w, unusableGrant())
		return
	}
	association, err := a.Store.redeemLinkGrant(r.Context(), subject, sha256.Sum256(raw), func(source string) bool {
		return a.Config.LinkingIssuers[source] != ""
	})
	if err != nil {
		fail(w, err)
		return
	}
	w.Header().Set("Location", associationPath+"/"+association.ID)
	writeJSON(w, 201, association)
}

func listAssociations(w http.ResponseWriter, r *http.Request, q *db.Queries, subject string) {
	cursor, limit, ok := parsePage(w, r)
	if !ok {
		return
	}
	if cursor == "" {
		cursor = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := q.ListActiveVerifiedAssociations(r.Context(), db.ListActiveVerifiedAssociationsParams{
		CustomerSubject: subject, ID: cursor, Limit: int32(limit + 1),
	})
	if err != nil {
		fail(w, err)
		return
	}
	page := struct {
		Items      []verifiedAssociation `json:"items"`
		NextCursor string                `json:"next_cursor,omitempty"`
	}{Items: make([]verifiedAssociation, 0, min(limit, len(rows)))}
	for _, row := range rows[:min(limit, len(rows))] {
		page.Items = append(page.Items, verifiedAssociation(row))
	}
	if len(rows) > limit {
		page.NextCursor = rows[limit-1].ID
	}
	writeJSON(w, 200, page)
}

func decodeLinkingJSON(w http.ResponseWriter, r *http.Request, destination any, fields ...string) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		writeProblem(w, problem(415, "Unsupported media type", "Use application/json."))
		return false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<10))
	if err != nil {
		writeProblem(w, problem(413, "Payload too large", "Request body must not exceed 4 KiB."))
		return false
	}
	object := bytes.TrimSpace(raw)
	if len(object) != 0 && object[0] == '{' && utf8.Valid(raw) && json.Valid(raw) && !duplicateKeys(raw) {
		var keys map[string]json.RawMessage
		_ = json.Unmarshal(raw, &keys)
		for key := range keys {
			if !slices.Contains(fields, key) {
				writeProblem(w, problem(400, "Invalid JSON", "Supply one JSON object with only the supported fields."))
				return false
			}
		}
		if json.Unmarshal(raw, destination) == nil {
			return true
		}
	}
	writeProblem(w, problem(400, "Invalid JSON", "Supply one JSON object with only the supported fields."))
	return false
}

func emptyLinkingBody(w http.ResponseWriter, r *http.Request) bool {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1))
	if err == nil && len(body) == 0 {
		return true
	}
	writeProblem(w, problem(400, "Invalid payload", "This resource does not accept a request body."))
	return false
}
