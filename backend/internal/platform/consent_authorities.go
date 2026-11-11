package platform

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"regexp"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var (
	controllerName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9 ._:/-]{0,127}$`)
	governanceRef   = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)
	receiptKeyID    = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`)
	maxReviewPeriod = 366 * 24 * time.Hour
)

// ConsentAuthority lets one source report consent grants for one purpose (ADR 0015, ADR 0017). It
// records the vetting behind it and the arrangement between source and controller, and authorizes
// nothing once its review date has passed, while suspended or once revoked. Approving a source is not
// verifying any of its grants: each grant it reports is the source's signed attestation.
type ConsentAuthority struct {
	ID                   string     `json:"id"`
	Source               string     `json:"source"`
	PurposeID            string     `json:"purpose_id"`
	Controller           string     `json:"controller"`
	CollectionMethods    []string   `json:"collection_methods"`
	ArrangementReference string     `json:"arrangement_reference,omitempty"`
	VettingReference     string     `json:"vetting_reference,omitempty"`
	ReviewBy             string     `json:"review_by,omitempty"`
	State                string     `json:"state"`
	ApprovedBy           string     `json:"approved_by"`
	ApprovedAt           time.Time  `json:"approved_at"`
	RevokedAt            *time.Time `json:"revoked_at,omitempty"`
	RevokedBy            string     `json:"revoked_by,omitempty"`
}

func consentAuthorityFromRow(r db.ConsentAuthority) ConsentAuthority {
	a := ConsentAuthority{ID: r.ID, Source: r.Source, PurposeID: r.PurposeID, Controller: r.Controller, CollectionMethods: r.CollectionMethods,
		ArrangementReference: r.ArrangementReference.String, VettingReference: r.VettingReference.String, State: r.State,
		ApprovedBy: r.ApprovedBy, ApprovedAt: r.ApprovedAt, RevokedBy: r.RevokedBy.String}
	if r.ReviewBy.Valid {
		a.ReviewBy = r.ReviewBy.Time.Format(time.DateOnly)
	}
	if r.RevokedAt.Valid {
		a.RevokedAt = &r.RevokedAt.Time
	}
	return a
}

// ConsentAuthorityRequest is what an operator approves.
type ConsentAuthorityRequest struct {
	Source            string   `json:"source"`
	PurposeID         string   `json:"purpose_id"`
	Controller        string   `json:"controller"`
	CollectionMethods []string `json:"collection_methods"`
	AuthorityReview
}

// AuthorityReview is the vetting an approval or renewal records: the reference of the source's vetting
// record, of the arrangement under which it collects consent for the controller, and the date by which
// it must be reviewed again (at most a year ahead).
type AuthorityReview struct {
	ArrangementReference string `json:"arrangement_reference"`
	VettingReference     string `json:"vetting_reference"`
	ReviewBy             string `json:"review_by"`
}

func (r AuthorityReview) parse() (pgtype.Date, error) {
	invalid := func(detail string) (pgtype.Date, error) {
		return pgtype.Date{}, problem(422, "Invalid consent authority", detail)
	}
	review, err := time.Parse(time.DateOnly, r.ReviewBy)
	switch {
	case !governanceRef.MatchString(r.ArrangementReference):
		return invalid("arrangement_reference must reference the source-controller arrangement.")
	case !governanceRef.MatchString(r.VettingReference):
		return invalid("vetting_reference must reference the record of the source's vetting.")
	case err != nil || !review.After(time.Now()) || review.After(time.Now().Add(maxReviewPeriod)):
		return invalid("review_by must be a date after today and within a year.")
	}
	return pgtype.Date{Time: review, Valid: true}, nil
}

// ApproveConsentAuthority lets a registered source report consent grants for a consent-based purpose, as
// collector for the named controller, by the given methods, after the vetting it references. One
// authority per source and purpose exists until it is revoked. Grants also need a receipt key.
func (s Store) ApproveConsentAuthority(ctx context.Context, req ConsentAuthorityRequest, actor string) (ConsentAuthority, error) {
	invalid := func(detail string) (ConsentAuthority, error) {
		return ConsentAuthority{}, problem(422, "Invalid consent authority", detail)
	}
	switch {
	case !profiles.Name.MatchString(req.Source):
		return invalid("source must name a registered source.")
	case !profiles.ID.MatchString(req.PurposeID):
		return invalid("purpose_id must be a consent purpose ID.")
	case !controllerName.MatchString(req.Controller):
		return invalid("controller must identify the controller the consent is collected for (1 to 128 characters).")
	case len(req.CollectionMethods) == 0 || !subset(req.CollectionMethods, collectionMethods):
		return invalid("collection_methods must name double_opt_in, signed_form, recorded_web_form or recorded_call.")
	}
	review, err := req.AuthorityReview.parse()
	if err != nil {
		return ConsentAuthority{}, err
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ConsentAuthority{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if registered, err := q.ListRegisteredSources(ctx, []string{req.Source}); err != nil {
		return ConsentAuthority{}, err
	} else if len(registered) != 1 {
		return invalid("source must name a registered source.")
	}
	active, err := q.GetActivePolicyForPurpose(ctx, req.PurposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid("The purpose needs an active policy version.")
	}
	if err != nil {
		return ConsentAuthority{}, err
	}
	if active.LawfulBasis != string(consent.Consent) {
		return invalid("Only a consent-based purpose collects consent.")
	}
	row, err := q.InsertConsentAuthority(ctx, db.InsertConsentAuthorityParams{ID: UUID(), Source: req.Source, PurposeID: req.PurposeID,
		Controller: req.Controller, CollectionMethods: dedupe(req.CollectionMethods), ApprovedBy: actor,
		ArrangementReference: pgtype.Text{String: req.ArrangementReference, Valid: true}, VettingReference: pgtype.Text{String: req.VettingReference, Valid: true},
		ReviewBy: review})
	var pgErr interface{ SQLState() string }
	if errors.As(err, &pgErr) && pgErr.SQLState() == "23505" {
		return ConsentAuthority{}, problem(409, "Authority exists", "This source already has an authority for this purpose; revoke it first.")
	}
	if err != nil {
		return ConsentAuthority{}, err
	}
	if err = auditAuthority(ctx, q, row.ID, "approved", "", req.VettingReference, actor); err != nil {
		return ConsentAuthority{}, err
	}
	return consentAuthorityFromRow(row), tx.Commit(ctx)
}

func auditAuthority(ctx context.Context, q *db.Queries, id, action, keyID, reference, actor string) error {
	return q.InsertAuthorityAudit(ctx, db.InsertAuthorityAuditParams{ID: UUID(), AuthorityID: id, Action: action,
		KeyID: pgtype.Text{String: keyID, Valid: keyID != ""}, Reference: pgtype.Text{String: reference, Valid: reference != ""}, Actor: actor})
}

// changeAuthority locks an authority and applies one audited change to it.
func (s Store) changeAuthority(ctx context.Context, id string, change func(*db.Queries, db.ConsentAuthority) (db.ConsentAuthority, error)) (ConsentAuthority, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ConsentAuthority{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	current, err := q.LockConsentAuthority(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ConsentAuthority{}, problem(404, "Not found", "Unknown consent authority.")
	}
	if err != nil {
		return ConsentAuthority{}, err
	}
	if current.State == "revoked" {
		return ConsentAuthority{}, problem(409, "Authority revoked", "The consent authority is revoked.")
	}
	row, err := change(q, current)
	if err != nil {
		return ConsentAuthority{}, err
	}
	return consentAuthorityFromRow(row), tx.Commit(ctx)
}

// RevokeConsentAuthority ends an authority for good. Grants made under it stop counting as consent at
// once; withdrawals and objections made under it keep counting.
func (s Store) RevokeConsentAuthority(ctx context.Context, id, actor string) (ConsentAuthority, error) {
	return s.changeAuthority(ctx, id, func(q *db.Queries, a db.ConsentAuthority) (db.ConsentAuthority, error) {
		row, err := q.RevokeConsentAuthority(ctx, db.RevokeConsentAuthorityParams{ID: a.ID, RevokedBy: pgtype.Text{String: actor, Valid: true}})
		if err != nil {
			return row, err
		}
		return row, auditAuthority(ctx, q, a.ID, "revoked", "", "", actor)
	})
}

// SuspendConsentAuthority stops an authority while its source is investigated: its grants stop
// counting as consent and it accepts none, until it is renewed.
func (s Store) SuspendConsentAuthority(ctx context.Context, id, reference, actor string) (ConsentAuthority, error) {
	if !governanceRef.MatchString(reference) {
		return ConsentAuthority{}, problem(422, "Invalid suspension", "reference must name the review or incident behind the suspension.")
	}
	return s.changeAuthority(ctx, id, func(q *db.Queries, a db.ConsentAuthority) (db.ConsentAuthority, error) {
		row, err := q.SuspendConsentAuthority(ctx, a.ID)
		if errors.Is(err, pgx.ErrNoRows) {
			return row, problem(409, "Not active", "Only an active authority can be suspended.")
		}
		if err != nil {
			return row, err
		}
		return row, auditAuthority(ctx, q, a.ID, "suspended", "", reference, actor)
	})
}

// RenewConsentAuthority records a new review of the source and makes the authority active until the new
// review date. It resumes a suspended authority, and one whose review date passed.
func (s Store) RenewConsentAuthority(ctx context.Context, id string, review AuthorityReview, actor string) (ConsentAuthority, error) {
	date, err := review.parse()
	if err != nil {
		return ConsentAuthority{}, err
	}
	return s.changeAuthority(ctx, id, func(q *db.Queries, a db.ConsentAuthority) (db.ConsentAuthority, error) {
		action := "renewed"
		if a.State == "suspended" {
			action = "resumed"
		}
		row, err := q.RenewConsentAuthority(ctx, db.RenewConsentAuthorityParams{ID: a.ID, VettingReference: pgtype.Text{String: review.VettingReference, Valid: true},
			ArrangementReference: pgtype.Text{String: review.ArrangementReference, Valid: true}, ReviewBy: date})
		if err != nil {
			return row, err
		}
		return row, auditAuthority(ctx, q, a.ID, action, "", review.VettingReference, actor)
	})
}

func (s Store) ConsentAuthorities(ctx context.Context) ([]ConsentAuthority, error) {
	rows, err := db.New(s.Pool).ListConsentAuthorities(ctx)
	out := make([]ConsentAuthority, 0, len(rows))
	for _, r := range rows {
		out = append(out, consentAuthorityFromRow(r))
	}
	return out, err
}

// AuthorityKey is a public key a source signs its consent receipts with. A retired key signs no new
// grants; a compromised key's grants all stop counting.
type AuthorityKey struct {
	KeyID     string     `json:"key_id"`
	PublicKey string     `json:"public_key"`
	State     string     `json:"state"`
	AddedBy   string     `json:"added_by"`
	AddedAt   time.Time  `json:"added_at"`
	ChangedAt *time.Time `json:"changed_at,omitempty"`
}

func authorityKeyFromRow(r db.ConsentAuthorityKey) AuthorityKey {
	k := AuthorityKey{KeyID: r.KeyID, PublicKey: base64.StdEncoding.EncodeToString(r.PublicKey), State: r.State, AddedBy: r.AddedBy, AddedAt: r.AddedAt}
	if r.ChangedAt.Valid {
		k.ChangedAt = &r.ChangedAt.Time
	}
	return k
}

// AuthorityKeyRequest registers a source's Ed25519 public key, base64-encoded.
type AuthorityKeyRequest struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// AddAuthorityKey registers a receipt key for an authority that is not revoked. Several keys can be
// active at once, so a source can rotate without a gap.
func (s Store) AddAuthorityKey(ctx context.Context, id string, req AuthorityKeyRequest, actor string) (AuthorityKey, error) {
	public, err := base64.StdEncoding.DecodeString(req.PublicKey)
	switch {
	case !receiptKeyID.MatchString(req.KeyID):
		return AuthorityKey{}, problem(422, "Invalid key", "key_id must be 1 to 64 letters, digits and ._:- characters.")
	case err != nil || len(public) != ed25519.PublicKeySize:
		return AuthorityKey{}, problem(422, "Invalid key", "public_key must be a base64 Ed25519 public key.")
	}
	var key AuthorityKey
	_, err = s.changeAuthority(ctx, id, func(q *db.Queries, a db.ConsentAuthority) (db.ConsentAuthority, error) {
		row, err := q.InsertAuthorityKey(ctx, db.InsertAuthorityKeyParams{ID: UUID(), AuthorityID: a.ID, KeyID: req.KeyID, PublicKey: public, AddedBy: actor})
		if errors.Is(err, pgx.ErrNoRows) {
			return a, problem(409, "Key exists", "This authority already has a key with this key_id.")
		}
		if err != nil {
			return a, err
		}
		key = authorityKeyFromRow(row)
		return a, auditAuthority(ctx, q, a.ID, "key_added", req.KeyID, "", actor)
	})
	return key, err
}

// SetAuthorityKeyState retires a key (it signs no new grants; grants it signed keep counting) or marks it
// compromised (every grant it signed stops counting at once). Compromise is final.
func (s Store) SetAuthorityKeyState(ctx context.Context, id, keyID, state, actor string) (AuthorityKey, error) {
	var key AuthorityKey
	_, err := s.changeAuthority(ctx, id, func(q *db.Queries, a db.ConsentAuthority) (db.ConsentAuthority, error) {
		if _, err := q.GetAuthorityKey(ctx, db.GetAuthorityKeyParams{AuthorityID: a.ID, KeyID: keyID}); errors.Is(err, pgx.ErrNoRows) {
			return a, problem(404, "Not found", "Unknown receipt key.")
		} else if err != nil {
			return a, err
		}
		row, err := q.SetAuthorityKeyState(ctx, db.SetAuthorityKeyStateParams{State: state, AuthorityID: a.ID, KeyID: keyID})
		if errors.Is(err, pgx.ErrNoRows) {
			return a, problem(409, "Key state", "Only an active key can be retired, and a compromised key stays compromised.")
		}
		if err != nil {
			return a, err
		}
		key = authorityKeyFromRow(row)
		action := map[string]string{"retired": "key_retired", "compromised": "key_compromised"}[state]
		return a, auditAuthority(ctx, q, a.ID, action, keyID, "", actor)
	})
	return key, err
}

func (s Store) AuthorityKeys(ctx context.Context, id string) ([]AuthorityKey, error) {
	q := db.New(s.Pool)
	if _, err := q.GetConsentAuthority(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Unknown consent authority.")
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListAuthorityKeys(ctx, id)
	out := make([]AuthorityKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, authorityKeyFromRow(r))
	}
	return out, err
}

// AuthorityAuditEntry is one audited change to an authority or its keys.
type AuthorityAuditEntry struct {
	Action     string    `json:"action"`
	KeyID      string    `json:"key_id,omitempty"`
	Reference  string    `json:"reference,omitempty"`
	Actor      string    `json:"actor"`
	RecordedAt time.Time `json:"recorded_at"`
}

func (s Store) AuthorityAudit(ctx context.Context, id string) ([]AuthorityAuditEntry, error) {
	q := db.New(s.Pool)
	if _, err := q.GetConsentAuthority(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Unknown consent authority.")
	} else if err != nil {
		return nil, err
	}
	rows, err := q.ListAuthorityAudit(ctx, id)
	out := make([]AuthorityAuditEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, AuthorityAuditEntry{Action: r.Action, KeyID: r.KeyID.String, Reference: r.Reference.String, Actor: r.Actor, RecordedAt: r.RecordedAt})
	}
	return out, err
}
