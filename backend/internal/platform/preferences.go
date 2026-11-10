package platform

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const customerPreferencesPath = customerProfilePath + "/preferences"

// CustomerPreferences are a customer's own fields at a version; version 0 means nothing is set yet.
type CustomerPreferences struct {
	profiles.Preferences
	Version   int64              `json:"version"`
	UpdatedAt *time.Time         `json:"updated_at,omitempty"`
	Changes   []PreferenceChange `json:"changes"`
}

// PreferenceChange names the fields one version changed, never their values.
type PreferenceChange struct {
	Version   int64     `json:"version"`
	Fields    []string  `json:"fields"`
	ChangedAt time.Time `json:"changed_at"`
}

func text(v pgtype.Text) *string {
	if !v.Valid {
		return nil
	}
	return &v.String
}

func pgText(v *string) pgtype.Text {
	if v == nil {
		return pgtype.Text{}
	}
	return pgtype.Text{String: *v, Valid: true}
}

func customerAnchor(ctx context.Context, q *db.Queries, kratos string) (string, error) {
	id, err := q.GetCustomerProfileID(ctx, kratos)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", problem(404, "Not found", "The customer profile has not been provisioned.")
	}
	return id, err
}

// Preferences returns the customer's own fields and their recent change log.
func (s Store) Preferences(ctx context.Context, kratos string) (CustomerPreferences, error) {
	result := CustomerPreferences{Changes: []PreferenceChange{}}
	q := db.New(s.Pool)
	anchor, err := customerAnchor(ctx, q, kratos)
	if err != nil {
		return result, err
	}
	row, err := q.GetCustomerPreferences(ctx, anchor)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, nil
	}
	if err != nil {
		return result, err
	}
	result.Preferences = profiles.Preferences{DisplayName: text(row.DisplayName), PreferredLanguage: text(row.PreferredLanguage), Timezone: text(row.Timezone)}
	result.Version, result.UpdatedAt = row.Version, &row.UpdatedAt
	changes, err := q.ListPreferenceChanges(ctx, anchor)
	for _, c := range changes {
		result.Changes = append(result.Changes, PreferenceChange{Version: c.Version, Fields: c.Fields, ChangedAt: c.ChangedAt})
	}
	return result, err
}

// SetPreferences replaces the customer's own fields. expected is the version the customer read (0 before
// the first write), so two devices cannot overwrite each other silently. An unchanged document keeps its
// version; a change records which fields changed.
func (s Store) SetPreferences(ctx context.Context, kratos string, p profiles.Preferences, expected int64) (CustomerPreferences, error) {
	p, err := p.Normalize()
	if err != nil {
		return CustomerPreferences{}, problem(422, "Invalid preferences", err.Error())
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return CustomerPreferences{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	anchor, err := customerAnchor(ctx, q, kratos)
	if err != nil {
		return CustomerPreferences{}, err
	}
	locked, err := q.LockProfile(ctx, anchor)
	if err != nil {
		return CustomerPreferences{}, err
	}
	if locked.LifecycleState != "active" || locked.Restricted {
		return CustomerPreferences{}, problem(409, "Profile not writable", "The customer profile's lifecycle state does not accept changes.")
	}
	current, err := q.LockCustomerPreferences(ctx, anchor)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CustomerPreferences{}, err
	}
	switch {
	case exists && expected == 0:
		return CustomerPreferences{}, problem(428, "Precondition required", "Supply the preferences ETag in If-Match.")
	case current.Version != expected:
		return CustomerPreferences{}, problem(412, "Stale preferences", "Read the preferences again and reapply the change.")
	}
	previous := profiles.Preferences{DisplayName: text(current.DisplayName), PreferredLanguage: text(current.PreferredLanguage), Timezone: text(current.Timezone)}
	if fields := p.Changed(previous); len(fields) > 0 {
		next := current.Version + 1
		if err = q.UpsertCustomerPreferences(ctx, db.UpsertCustomerPreferencesParams{AnchorProfileID: anchor, DisplayName: pgText(p.DisplayName),
			PreferredLanguage: pgText(p.PreferredLanguage), Timezone: pgText(p.Timezone), Version: next}); err != nil {
			return CustomerPreferences{}, err
		}
		if err = q.InsertPreferenceChange(ctx, db.InsertPreferenceChangeParams{AnchorProfileID: anchor, Version: next, Fields: fields}); err != nil {
			return CustomerPreferences{}, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return CustomerPreferences{}, err
	}
	return s.Preferences(ctx, kratos)
}

// serveCustomerPreferences reads or replaces the session customer's own fields. Nothing in the request can
// name another customer, and these fields never reach source-owned attributes or Kratos traits.
func (a *API) serveCustomerPreferences(w http.ResponseWriter, r *http.Request) {
	if !allow(w, r, http.MethodGet, http.MethodPut) {
		return
	}
	session, ok := a.authenticateCustomer(w, r)
	if !ok {
		return
	}
	if r.URL.RawQuery != "" {
		writeProblem(w, problem(400, "Invalid query", "The session determines the customer."))
		return
	}
	var result CustomerPreferences
	var err error
	if r.Method == http.MethodGet {
		result, err = a.Store.Preferences(r.Context(), session.Subject)
	} else {
		expected, ok := expectedRevision(w, r)
		if !ok {
			return
		}
		var body profiles.Preferences
		if !decodeJSON(w, r, &body) {
			return
		}
		result, err = a.Store.SetPreferences(r.Context(), session.Subject, body, int64(expected))
	}
	if err != nil {
		fail(w, err)
		return
	}
	// Before the first write there is no version to match; the first PUT omits If-Match.
	if result.Version > 0 {
		w.Header().Set("ETag", `"`+strconv.FormatInt(result.Version, 10)+`"`)
	}
	writeJSON(w, 200, result)
}
