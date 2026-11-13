package platform

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

// Unified customer profiles (ADR 0018). A source profile is one source's record and only that source
// writes it. The identity graph links source profiles that a trusted identity key says are the same
// customer; connected profiles form one unified customer named by its oldest member. The reconciler keeps
// the graph current from the reconciliation queue, which every relevant change feeds in its own
// transaction.

// errOutsideScope answers a source write to a profile it has no record in. An unknown profile gets the
// same answer, so a profile ID reveals nothing.
var errOutsideScope = problem(403, "Forbidden", "This profile is outside the integration source's scope.")

// routeWrite returns the source's own record that a write to id is meant for: id itself when the source
// created it, otherwise the source's one record in the unified customer id belongs to.
func routeWrite(ctx context.Context, q *db.Queries, id, source string) (string, error) {
	creator, err := q.RecordCreator(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", errOutsideScope
	}
	if err != nil {
		return "", err
	}
	if creator.Valid && creator.String == source {
		return id, nil
	}
	canonical, err := q.CanonicalOf(ctx, id)
	if err != nil {
		return "", err
	}
	records, err := q.SourceRecordsOfCustomer(ctx, db.SourceRecordsOfCustomerParams{Source: source, CanonicalID: canonical})
	switch {
	case err != nil:
		return "", err
	case len(records) == 0:
		return "", errOutsideScope
	case len(records) > 1:
		return "", problem(409, "Ambiguous record", "The integration source has several records in this customer; write one by its own ID.")
	}
	return records[0], nil
}

const reconcilerActor = "identity-reconciler"

// ReconcileIdentities works through the reconciliation queue, one profile per transaction, until it is
// empty or ctx ends, and returns how many profiles it reconciled.
func (s Store) ReconcileIdentities(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		done, err := s.reconcileNext(ctx)
		if err != nil || !done {
			return n, err
		}
		n++
	}
	return n, ctx.Err()
}

func (s Store) reconcileNext(ctx context.Context) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockIdentityGraph(ctx); err != nil {
		return false, err
	}
	id, err := q.ClaimReconciliation(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = reconcileProfile(ctx, q, id); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

type linkKey struct{ a, b, namespace string }

func pair(x, y, namespace string) linkKey {
	if y < x {
		x, y = y, x
	}
	return linkKey{x, y, namespace}
}

// reconcileProfile recomputes one profile's links from current state under the graph lock. A link is
// wanted when a trusted key names both profiles, the pair was not corrected apart for that key, and
// joining their customers gives no trusted key two values. Wanted links are added in a fixed order, so a
// repeated step decides the same; links no longer wanted are removed, and every customer touched is
// recalculated. Removing links requeues the customers concerned and the profiles once refused for
// conflicting with them, so a link held back only by a removed one is made, and the graph settles.
func reconcileProfile(ctx context.Context, q *db.Queries, id string) error {
	profile, err := q.ReconcilableProfile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	type candidate struct{ namespace, holder string }
	var candidates []candidate
	if profile.Linkable {
		keys, err := q.TrustedKeysForSource(ctx, profile.CreatorSource.String)
		if err != nil {
			return err
		}
		trusted := map[string][]string{}
		namespaces := make([]string, 0, len(keys))
		for _, k := range keys {
			trusted[k.Namespace] = k.Sources
			namespaces = append(namespaces, k.Namespace)
		}
		values, err := q.ProfileKeyValues(ctx, db.ProfileKeyValuesParams{ProfileID: id, Namespaces: namespaces})
		if err != nil {
			return err
		}
		for _, v := range values {
			holders, err := q.KeyHolders(ctx, db.KeyHoldersParams{Namespace: v.Namespace, Value: v.Value, ProfileID: id, Sources: trusted[v.Namespace]})
			if err != nil {
				return err
			}
			for _, h := range holders {
				candidates = append(candidates, candidate{v.Namespace, h})
			}
		}
	}
	slices.SortFunc(candidates, func(x, y candidate) int {
		return cmp.Or(cmp.Compare(x.namespace, y.namespace), cmp.Compare(x.holder, y.holder))
	})
	candidates = slices.Compact(candidates)
	// own is the customer the profile forms with the links kept so far; other customers are traversed
	// without passing through the profile, so its current links decide nothing.
	own := []string{id}
	wanted := map[linkKey]bool{}
	for _, c := range candidates {
		link := pair(id, c.holder, c.namespace)
		corrected, err := q.CorrectedLink(ctx, db.CorrectedLinkParams{ProfileA: link.a, ProfileB: link.b, KeyNamespace: c.namespace})
		if err != nil {
			return err
		}
		if corrected {
			continue
		}
		if !slices.Contains(own, c.holder) {
			other, err := component(ctx, q, c.holder, id)
			if err != nil {
				return err
			}
			joined := append(slices.Clone(own), other...)
			contradictory, err := contradicts(ctx, q, joined)
			if err != nil {
				return err
			}
			if contradictory {
				if err = recordConflict(ctx, q, id, c.holder, c.namespace); err != nil {
					return err
				}
				continue
			}
			own = joined
		}
		wanted[link] = true
	}
	existing, err := q.ActiveLinksOf(ctx, id)
	if err != nil {
		return err
	}
	touched := []string{id}
	removed := false
	reason := "key_changed"
	if !profile.Linkable {
		reason = "profile_unavailable"
	}
	for _, l := range existing {
		k := linkKey{l.ProfileA, l.ProfileB, l.KeyNamespace}
		if wanted[k] {
			delete(wanted, k)
			continue
		}
		if err = removeLink(ctx, q, l, reconcilerActor, reason, ""); err != nil {
			return err
		}
		removed = true
		touched = append(touched, l.ProfileA, l.ProfileB)
	}
	added := slices.SortedFunc(func(yield func(linkKey) bool) {
		for k := range wanted {
			if !yield(k) {
				return
			}
		}
	}, func(x, y linkKey) int {
		return cmp.Or(cmp.Compare(x.namespace, y.namespace), cmp.Compare(x.a, y.a), cmp.Compare(x.b, y.b))
	})
	for _, k := range added {
		linkID := UUID()
		if err = q.InsertProfileLink(ctx, db.InsertProfileLinkParams{ID: linkID, ProfileA: k.a, ProfileB: k.b, KeyNamespace: k.namespace}); err != nil {
			return err
		}
		for _, end := range [][2]string{{k.a, k.b}, {k.b, k.a}} {
			if err = decide(ctx, q, end[0], "linked", k.namespace, end[1], linkID, "", reconcilerActor); err != nil {
				return err
			}
		}
		touched = append(touched, k.a, k.b)
	}
	members, err := recalculateCustomers(ctx, q, touched)
	if err != nil || !removed {
		return err
	}
	return q.RequeueAfterRemoval(ctx, db.RequeueAfterRemovalParams{Ids: members, CurrentID: id})
}

// component returns the profiles connected to start by active links, not traversing through skip.
func component(ctx context.Context, q *db.Queries, start, skip string) ([]string, error) {
	seen := []string{start}
	frontier := []string{start}
	for len(frontier) > 0 {
		next, err := q.Neighbours(ctx, frontier)
		if err != nil {
			return nil, err
		}
		frontier = frontier[:0]
		for _, n := range next {
			if n != skip && !slices.Contains(seen, n) {
				seen = append(seen, n)
				frontier = append(frontier, n)
			}
		}
	}
	return seen, nil
}

// contradicts reports whether the profiles together hold two values of one trusted key.
func contradicts(ctx context.Context, q *db.Queries, ids []string) (bool, error) {
	counts, err := q.ComponentKeyValues(ctx, ids)
	if err != nil {
		return false, err
	}
	return slices.ContainsFunc(counts, func(c db.ComponentKeyValuesRow) bool { return c.Values > 1 }), nil
}

// recordConflict records a refused link once for as long as it stays refused.
func recordConflict(ctx context.Context, q *db.Queries, id, other, namespace string) error {
	last, err := q.LastPairDecision(ctx, db.LastPairDecisionParams{ProfileID: id, RelatedProfileID: other, KeyNamespace: namespace})
	if err != nil || last == "conflict" {
		return err
	}
	return decide(ctx, q, id, "conflict", namespace, other, "", "contradictory_key", reconcilerActor)
}

func removeLink(ctx context.Context, q *db.Queries, l db.ProfileLink, actor, reason, reference string) error {
	if err := q.RemoveProfileLink(ctx, db.RemoveProfileLinkParams{ID: l.ID, RemovedBy: actor, RemovalReason: reason, RemovalReference: reference}); err != nil {
		return err
	}
	for _, end := range [][2]string{{l.ProfileA, l.ProfileB}, {l.ProfileB, l.ProfileA}} {
		if err := decide(ctx, q, end[0], "unlinked", l.KeyNamespace, end[1], l.ID, reason, actor); err != nil {
			return err
		}
	}
	return nil
}

func decide(ctx context.Context, q *db.Queries, id, action, namespace, related, linkID, reason, actor string) error {
	return q.InsertIdentityDecision(ctx, db.InsertIdentityDecisionParams{ID: UUID(), ProfileID: id, Action: action, KeyNamespace: namespace,
		RelatedProfileID: related, LinkID: linkID, Reason: reason, Actor: actor})
}

// recalculateCustomers names every customer the given profiles now belong to by its oldest member, and
// returns those customers' members. A profile whose canonical ID changes records the decision and emits
// profile.unified.v1.
func recalculateCustomers(ctx context.Context, q *db.Queries, ids []string) ([]string, error) {
	done := map[string]bool{}
	var all []string
	for _, id := range ids {
		if done[id] {
			continue
		}
		members, err := component(ctx, q, id, "")
		if err != nil {
			return nil, err
		}
		canonical, err := q.OldestProfile(ctx, members)
		if err != nil {
			return nil, err
		}
		slices.Sort(members)
		all = append(all, members...)
		for _, m := range members {
			done[m] = true
			previous, err := q.CanonicalOf(ctx, m)
			if err != nil {
				return nil, err
			}
			if previous == canonical {
				continue
			}
			if m == canonical {
				err = q.ClearAlias(ctx, m)
			} else {
				err = q.SetAlias(ctx, db.SetAliasParams{ProfileID: m, CanonicalID: canonical})
			}
			if err != nil {
				return nil, err
			}
			if err = decide(ctx, q, m, "canonical_changed", "", canonical, "", "", reconcilerActor); err != nil {
				return nil, err
			}
			if err = q.AppendUnifiedEvent(ctx, db.AppendUnifiedEventParams{ID: UUID(), ProfileID: m, CanonicalID: canonical}); err != nil {
				return nil, err
			}
		}
	}
	return all, nil
}

// RemoveLink is an identity reviewer's correction: the link goes at once, the customers it joined are
// recalculated, and the reconciler never links the pair by that key again.
func (s Store) RemoveLink(ctx context.Context, linkID, actor, reference string) (ProfileLink, error) {
	if !governanceRef.MatchString(reference) {
		return ProfileLink{}, problem(422, "Invalid correction", "reference must be 1 to 128 letters, digits or ._:/- characters.")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ProfileLink{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockIdentityGraph(ctx); err != nil {
		return ProfileLink{}, err
	}
	l, err := q.GetProfileLink(ctx, linkID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ProfileLink{}, problem(404, "Not found", "Unknown profile link.")
	}
	if err != nil {
		return ProfileLink{}, err
	}
	if l.RemovedAt.Valid {
		return ProfileLink{}, problem(409, "Link removed", "This link was already removed.")
	}
	if err = removeLink(ctx, q, l, actor, "corrected", reference); err != nil {
		return ProfileLink{}, err
	}
	members, err := recalculateCustomers(ctx, q, []string{l.ProfileA, l.ProfileB})
	if err != nil {
		return ProfileLink{}, err
	}
	// Links the removed one held back may now be possible.
	if err = q.RequeueAfterRemoval(ctx, db.RequeueAfterRemovalParams{Ids: members, CurrentID: "00000000-0000-0000-0000-000000000000"}); err != nil {
		return ProfileLink{}, err
	}
	if l, err = q.GetProfileLink(ctx, linkID); err != nil {
		return ProfileLink{}, err
	}
	return profileLink(l), tx.Commit(ctx)
}

// IdentityReconciler drains the reconciliation queue in the API process.
type IdentityReconciler struct{ Store Store }

// Run reconciles every interval until ctx ends. A failed pass is logged and retried on the next tick;
// the queue keeps every unreconciled profile.
func (r IdentityReconciler) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := r.Store.ReconcileIdentities(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "identity reconciliation failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// ResolutionKey is a trusted identity key: an identifier namespace that names the same customer across
// its sources.
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

// SetResolutionKey declares or replaces a trusted key and queues every profile it may concern, so
// existing records are resolved by it too.
func (s Store) SetResolutionKey(ctx context.Context, namespace string, sources []string, actor string) (ResolutionKey, error) {
	sources = slices.Compact(slices.Sorted(slices.Values(sources)))
	if !profiles.Name.MatchString(namespace) || len(sources) < 2 || len(sources) > 20 {
		return ResolutionKey{}, problem(422, "Invalid key", "A trusted identity key names an identifier namespace and 2 to 20 distinct sources.")
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return ResolutionKey{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	unknown, err := q.UnknownSources(ctx, sources)
	if err != nil {
		return ResolutionKey{}, err
	}
	if len(unknown) > 0 {
		return ResolutionKey{}, problem(422, "Invalid key", "Every source must be a configured source.")
	}
	row, err := q.SetResolutionKey(ctx, db.SetResolutionKeyParams{Namespace: namespace, Sources: sources, ChangedBy: actor})
	if err != nil {
		return ResolutionKey{}, err
	}
	if _, err = q.EnqueueNamespace(ctx, namespace); err != nil {
		return ResolutionKey{}, err
	}
	return ResolutionKey{Namespace: row.Namespace, Sources: row.Sources, ChangedBy: row.ChangedBy, ChangedAt: row.ChangedAt}, tx.Commit(ctx)
}

// DeleteResolutionKey stops resolving by a key; the reconciler removes the links it supported.
func (s Store) DeleteResolutionKey(ctx context.Context, namespace string) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	n, err := q.DeleteResolutionKey(ctx, namespace)
	if err != nil {
		return err
	}
	if n == 0 {
		return problem(404, "Not found", "Unknown trusted identity key.")
	}
	if _, err = q.EnqueueNamespace(ctx, namespace); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ResolutionPolicy is an attribute's configured resolution rule.
type ResolutionPolicy struct {
	Path           string    `json:"path"`
	Strategy       string    `json:"strategy"`
	SourcePriority []string  `json:"source_priority"`
	ChangedBy      string    `json:"changed_by"`
	ChangedAt      time.Time `json:"changed_at"`
}

func resolutionPolicy(r db.AttributeResolutionPolicy) ResolutionPolicy {
	return ResolutionPolicy{Path: r.Path, Strategy: r.Strategy, SourcePriority: r.SourcePriority, ChangedBy: r.ChangedBy, ChangedAt: r.ChangedAt}
}

// ResolutionPolicies lists the configured policies.
func (s Store) ResolutionPolicies(ctx context.Context) ([]ResolutionPolicy, error) {
	rows, err := db.New(s.Pool).ListResolutionPolicies(ctx)
	out := make([]ResolutionPolicy, 0, len(rows))
	for _, r := range rows {
		out = append(out, resolutionPolicy(r))
	}
	return out, err
}

// SetResolutionPolicy declares or replaces an attribute's policy. Resolution happens on every read, so
// it applies to every customer at once.
func (s Store) SetResolutionPolicy(ctx context.Context, path, strategy string, priority []string, actor string) (ResolutionPolicy, error) {
	invalid := problem(422, "Invalid policy", "A policy is most_recent, or source_priority with 1 to 20 distinct configured sources.")
	if priority == nil {
		priority = []string{}
	}
	if !profiles.Name.MatchString(path) || !slices.Contains(profiles.Strategies, strategy) || len(priority) > 20 ||
		(strategy == "source_priority") != (len(priority) > 0) || len(slices.Compact(slices.Sorted(slices.Values(priority)))) != len(priority) {
		return ResolutionPolicy{}, invalid
	}
	q := db.New(s.Pool)
	unknown, err := q.UnknownSources(ctx, priority)
	if err != nil {
		return ResolutionPolicy{}, err
	}
	if len(unknown) > 0 {
		return ResolutionPolicy{}, invalid
	}
	row, err := q.SetResolutionPolicy(ctx, db.SetResolutionPolicyParams{Path: path, Strategy: strategy, SourcePriority: priority, ChangedBy: actor})
	return resolutionPolicy(row), err
}

// DeleteResolutionPolicy returns an attribute to the default: the earliest value still held.
func (s Store) DeleteResolutionPolicy(ctx context.Context, path string) error {
	n, err := db.New(s.Pool).DeleteResolutionPolicy(ctx, path)
	if err == nil && n == 0 {
		return problem(404, "Not found", "No policy for this attribute.")
	}
	return err
}

// ProfileLink is one identity link with its history.
type ProfileLink struct {
	ID               string     `json:"id"`
	Profiles         [2]string  `json:"profile_ids"`
	KeyNamespace     string     `json:"key_namespace"`
	CreatedAt        time.Time  `json:"created_at"`
	RemovedAt        *time.Time `json:"removed_at,omitempty"`
	RemovedBy        string     `json:"removed_by,omitempty"`
	RemovalReason    string     `json:"removal_reason,omitempty"`
	RemovalReference string     `json:"removal_reference,omitempty"`
}

func profileLink(l db.ProfileLink) ProfileLink {
	out := ProfileLink{ID: l.ID, Profiles: [2]string{l.ProfileA, l.ProfileB}, KeyNamespace: l.KeyNamespace, CreatedAt: l.CreatedAt,
		RemovedBy: l.RemovedBy.String, RemovalReason: l.RemovalReason.String, RemovalReference: l.RemovalReference.String}
	if l.RemovedAt.Valid {
		out.RemovedAt = &l.RemovedAt.Time
	}
	return out
}

// IdentityDecision is one entry of the identity audit trail.
type IdentityDecision struct {
	ID               string    `json:"id"`
	ProfileID        string    `json:"profile_id"`
	Action           string    `json:"action"`
	KeyNamespace     string    `json:"key_namespace,omitempty"`
	RelatedProfileID string    `json:"related_profile_id,omitempty"`
	LinkID           string    `json:"link_id,omitempty"`
	Reason           string    `json:"reason,omitempty"`
	Actor            string    `json:"actor"`
	DecidedAt        time.Time `json:"decided_at"`
}

// customerOf returns the members of the unified customer id belongs to; 404 for an unknown profile.
func customerOf(ctx context.Context, q *db.Queries, id string) ([]string, error) {
	if _, err := q.RecordCreator(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Profile does not exist.")
	} else if err != nil {
		return nil, err
	}
	canonical, err := q.CanonicalOf(ctx, id)
	if err != nil {
		return nil, err
	}
	members, err := q.CustomerMembers(ctx, canonical)
	ids := make([]string, 0, len(members))
	for _, m := range members {
		ids = append(ids, m.ID)
	}
	return ids, err
}

// IdentityHistory lists the identity links and decisions of the unified customer a profile belongs to,
// including links already removed.
func (s Store) IdentityHistory(ctx context.Context, id string) ([]ProfileLink, []IdentityDecision, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, nil, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	members, err := customerOf(ctx, q, id)
	if err != nil {
		return nil, nil, err
	}
	links, err := q.ListCustomerLinks(ctx, members)
	if err != nil {
		return nil, nil, err
	}
	decisions, err := q.ListIdentityDecisions(ctx, members)
	if err != nil {
		return nil, nil, err
	}
	outLinks := make([]ProfileLink, 0, len(links))
	for _, l := range links {
		outLinks = append(outLinks, profileLink(l))
	}
	outDecisions := make([]IdentityDecision, 0, len(decisions))
	for _, d := range decisions {
		outDecisions = append(outDecisions, IdentityDecision{ID: d.ID, ProfileID: d.ProfileID, Action: d.Action, KeyNamespace: d.KeyNamespace.String,
			RelatedProfileID: uuidString(d.RelatedProfileID), LinkID: uuidString(d.LinkID), Reason: d.Reason.String, Actor: d.Actor, DecidedAt: d.DecidedAt})
	}
	return outLinks, outDecisions, tx.Commit(ctx)
}
