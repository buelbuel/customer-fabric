package platform

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

// Identity resolution for unified profiles (ADR 0018). A profile has no owning source. Each source keeps
// its own namespaced identifiers and writes any profile it is associated with: one it created, or one it
// holds an identifier on. A source joins an existing profile only through resolution on a trusted
// identity key, never by naming a profile.

// errOutsideScope answers a source write to a profile it is not associated with. An unknown profile gets
// the same answer, so a profile ID reveals nothing.
var errOutsideScope = problem(403, "Forbidden", "This profile is outside the integration source's scope.")

// authorizeWriter takes the profile's row lock and decides whether the source is associated with it. No
// source writes a customer's profile anchor. The answer holds until the caller's transaction ends.
func authorizeWriter(ctx context.Context, q *db.Queries, id, source string) error {
	creator, err := q.LockProfileForAuthorization(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && !creator.Valid) {
		return errOutsideScope
	}
	if err != nil {
		return err
	}
	associated, err := q.SourceAssociatedWithProfile(ctx, db.SourceAssociatedWithProfileParams{ProfileID: id, Source: source})
	if err != nil {
		return err
	}
	if !associated {
		return errOutsideScope
	}
	return nil
}

// identityResolution is the decision for one new record that presented a trusted key.
type identityResolution struct {
	keyNamespace string
	// profileID is the profile the record joins; empty when it gets its own.
	profileID string
	outcome   string
}

// resolveIdentity looks the new record's trusted identity keys up, each under its own lock, and returns
// the one profile they all name. No match, or matches naming different profiles, leave the record its
// own profile: an ambiguous match is recorded, never merged. Identifiers in other namespaces never
// resolve: a source cannot reach another source's record by naming it.
func resolveIdentity(ctx context.Context, q *db.Queries, source string, identifiers []profiles.IdentifierInput) (identityResolution, error) {
	keys, err := q.ResolutionKeysForSource(ctx, source)
	if err != nil || len(keys) == 0 {
		return identityResolution{}, err
	}
	trusted := map[string][]string{}
	for _, k := range keys {
		trusted[k.Namespace] = k.Sources
	}
	var r identityResolution
	found := map[string]bool{}
	// Keys are locked in one order, so concurrent records presenting the same keys cannot deadlock.
	ordered := slices.SortedFunc(slices.Values(identifiers), func(a, b profiles.IdentifierInput) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Value, b.Value))
	})
	for _, identifier := range ordered {
		sources, ok := trusted[identifier.Namespace]
		if !ok {
			continue
		}
		if r.keyNamespace == "" {
			r.keyNamespace = identifier.Namespace
		}
		if err = q.LockIdentityKey(ctx, db.LockIdentityKeyParams{Namespace: identifier.Namespace, Value: identifier.Value}); err != nil {
			return r, err
		}
		matches, err := q.ProfilesWithIdentityKey(ctx, db.ProfilesWithIdentityKeyParams{Namespace: identifier.Namespace, Value: identifier.Value, Sources: sources})
		if err != nil {
			return r, err
		}
		for _, id := range matches {
			found[id] = true
		}
	}
	switch {
	case r.keyNamespace == "":
		return r, nil
	case len(found) == 0:
		r.outcome = "created"
	case len(found) == 1:
		for id := range found {
			r.profileID = id
		}
		r.outcome = "associated"
	default:
		r.outcome = "ambiguous"
	}
	return r, nil
}

// record keeps the decision for an accepted write, with the profile the record ended up on.
func (r identityResolution) record(ctx context.Context, q *db.Queries, mutationID, source, profileID string) error {
	if r.outcome == "" {
		return nil
	}
	return q.InsertIdentityResolution(ctx, db.InsertIdentityResolutionParams{ID: UUID(), MutationID: mutationID, Source: source,
		ProfileID: profileID, Outcome: r.outcome, KeyNamespace: r.keyNamespace})
}

// ResolutionKey is a trusted identity key: an identifier namespace that names the same person across its
// sources.
type ResolutionKey struct {
	Namespace string    `json:"namespace"`
	Sources   []string  `json:"sources"`
	ChangedBy string    `json:"changed_by"`
	ChangedAt time.Time `json:"changed_at"`
}

// ResolutionKeys lists the trusted keys.
func (s Store) ResolutionKeys(ctx context.Context) ([]ResolutionKey, error) {
	rows, err := db.New(s.Pool).ListResolutionKeys(ctx)
	out := make([]ResolutionKey, 0, len(rows))
	for _, r := range rows {
		out = append(out, ResolutionKey{Namespace: r.Namespace, Sources: r.Sources, ChangedBy: r.ChangedBy, ChangedAt: r.ChangedAt})
	}
	return out, err
}

// SetResolutionKey declares or replaces a trusted key. It applies to records created from now on; it
// never moves an existing record.
func (s Store) SetResolutionKey(ctx context.Context, namespace string, sources []string, actor string) (ResolutionKey, error) {
	sources = slices.Compact(slices.Sorted(slices.Values(sources)))
	if !profiles.Name.MatchString(namespace) || len(sources) < 2 || len(sources) > 20 {
		return ResolutionKey{}, problem(422, "Invalid key", "A trusted identity key names an identifier namespace and 2 to 20 distinct sources.")
	}
	q := db.New(s.Pool)
	unknown, err := q.UnknownSources(ctx, sources)
	if err != nil {
		return ResolutionKey{}, err
	}
	if len(unknown) > 0 {
		return ResolutionKey{}, problem(422, "Invalid key", "Every source must be a configured source.")
	}
	row, err := q.SetResolutionKey(ctx, db.SetResolutionKeyParams{Namespace: namespace, Sources: sources, ChangedBy: actor})
	return ResolutionKey{Namespace: row.Namespace, Sources: row.Sources, ChangedBy: row.ChangedBy, ChangedAt: row.ChangedAt}, err
}

// DeleteResolutionKey stops resolving by a key. Records already joined stay where they are.
func (s Store) DeleteResolutionKey(ctx context.Context, namespace string) error {
	n, err := db.New(s.Pool).DeleteResolutionKey(ctx, namespace)
	if err == nil && n == 0 {
		return problem(404, "Not found", "Unknown trusted identity key.")
	}
	return err
}

// IdentityResolution is one recorded decision.
type IdentityResolution struct {
	ID           string    `json:"id"`
	MutationID   string    `json:"mutation_id"`
	Source       string    `json:"source"`
	Outcome      string    `json:"outcome"`
	KeyNamespace string    `json:"key_namespace"`
	DecidedAt    time.Time `json:"decided_at"`
}

// IdentityResolutions lists the decisions that put records on a profile.
func (s Store) IdentityResolutions(ctx context.Context, profileID string) ([]IdentityResolution, error) {
	rows, err := db.New(s.Pool).ListIdentityResolutions(ctx, profileID)
	out := make([]IdentityResolution, 0, len(rows))
	for _, r := range rows {
		out = append(out, IdentityResolution{ID: r.ID, MutationID: r.MutationID, Source: r.Source, Outcome: r.Outcome, KeyNamespace: r.KeyNamespace, DecidedAt: r.DecidedAt})
	}
	return out, err
}
