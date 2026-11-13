package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	Pool *pgxpool.Pool
	// EvaluationTimeout bounds one population-scale evaluation; zero means defaultEvaluationTimeout.
	EvaluationTimeout time.Duration
	// Keys seal webhook signing secrets at rest. Without them no destination can be registered and
	// nothing is signed.
	Keys SealingKeys
}
type Problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func (p *Problem) Error() string { return p.Title }
func problem(status int, title, detail string) *Problem {
	return &Problem{Type: "about:blank", Status: status, Title: title, Detail: detail}
}
func UUID() string {
	var b [16]byte
	if _, e := rand.Read(b[:]); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func dbError(err error) error {
	var p *pgconn.PgError
	if errors.As(err, &p) && p.Code == "23505" {
		return problem(409, "Identifier conflict", "The source-scoped identifier is already associated with a profile.")
	}
	return err
}

// Mutate serializes keys and profile writes in one transaction. Only receipts, not attribute values, are retained for replay.
func (s Store) Mutate(ctx context.Context, source, key, hash, id string, version int64, m profiles.Mutation) (int, []byte, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, nil, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockMutationKey(ctx, source+":"+key); err != nil {
		return 0, nil, err
	}
	previous, err := q.GetMutation(ctx, db.GetMutationParams{Source: source, Key: key})
	if err == nil {
		if previous.RequestHash != hash {
			return 0, nil, problem(409, "Idempotency conflict", "Use a new key for a different request.")
		}
		return int(previous.Status), previous.Result, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return 0, nil, err
	}
	var expected *int64
	if id != "" {
		expected = &version
	}
	result, err := applyMutation(ctx, q, source, key, hash, id, expected, m)
	if err != nil {
		return 0, nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return result.Status, result.Body, nil
}

// lockWritableProfile is the lifecycle fence: every source write passes it after taking the row lock.
func lockWritableProfile(ctx context.Context, q *db.Queries, id string) (db.LockProfileRow, error) {
	locked, err := q.LockProfile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return locked, problem(404, "Not found", "Profile does not exist.")
	}
	if err != nil {
		return locked, err
	}
	if locked.LifecycleState != "active" || locked.Restricted {
		return locked, problem(409, "Profile not writable", "This profile's lifecycle state does not accept source writes.")
	}
	return locked, nil
}

type mutationResult struct {
	Status     int
	Body       []byte
	MutationID string
	Version    int64
}

// applyMutation writes a source profile, its contributions, provenance and outbox record through the
// caller's transaction, so they commit together (ADR 0018). A source writes only its own profiles: id may
// name the unified customer, and routes to the source's record in it. A nil expected version accepts the
// locked current version. Overlapping values of other sources never conflict: they live in their own
// profiles, and resolution decides what the unified customer shows.
func applyMutation(ctx context.Context, q *db.Queries, source, key, hash, id string, expected *int64, m profiles.Mutation) (mutationResult, error) {
	var err error
	create := id == ""
	attrs := map[string]json.RawMessage{}
	current := int64(0)
	if create {
		id = UUID()
		if err = q.CreateSourceProfile(ctx, db.CreateSourceProfileParams{ID: id, Source: source}); err != nil {
			return mutationResult{}, err
		}
	} else {
		if id, err = routeWrite(ctx, q, id, source); err != nil {
			return mutationResult{}, err
		}
		locked, err := lockWritableProfile(ctx, q, id)
		if err != nil {
			return mutationResult{}, err
		}
		current = locked.Version
		if expected != nil && current != *expected {
			return mutationResult{}, problem(412, "Version conflict", "Fetch the profile and retry with its current ETag and a new key.")
		}
		if err = json.Unmarshal(locked.Attributes, &attrs); err != nil {
			return mutationResult{}, err
		}
	}
	// Types are checked after the profile row lock, as on the event path, so a queued definition cannot
	// deadlock them.
	revisions, err := checkTypedValues(ctx, q, m.Set)
	if err != nil {
		return mutationResult{}, err
	}
	mid := UUID()
	status := 200
	if create {
		status = 201
	}
	next := current + 1
	body, _ := json.Marshal(profiles.Receipt{ID: id, Version: next, MutationID: mid})
	if err = q.RecordMutation(ctx, db.RecordMutationParams{ID: mid, Source: source, Key: key, RequestHash: hash, ProfileID: id, Status: int32(status), Result: body}); err != nil {
		return mutationResult{}, err
	}
	paths := m.Paths()
	decided := make([]int32, len(paths))
	for i, path := range paths {
		decided[i] = revisions[path]
	}
	if err = q.RecordProvenance(ctx, db.RecordProvenanceParams{ProfileID: id, Source: source, MutationID: mid, Outcome: "accepted", Paths: paths, Revisions: decided}); err != nil {
		return mutationResult{}, err
	}
	for k, v := range m.Set {
		attrs[k] = v
	}
	for _, k := range m.Remove {
		delete(attrs, k)
	}
	if len(attrs) > 100 {
		return mutationResult{}, problem(422, "Profile limit", "A profile may contain at most 100 attributes.")
	}
	tombstones := make([]bool, len(paths))
	for i, path := range paths {
		_, present := attrs[path]
		tombstones[i] = !present
	}
	if err = q.SetContributions(ctx, db.SetContributionsParams{ProfileID: id, Source: source, Paths: paths, Tombstones: tombstones}); err != nil {
		return mutationResult{}, err
	}
	for _, i := range m.Identifiers {
		// The unique constraint prevents concurrent claims, including claims by different profiles.
		owner, err := q.AssociateIdentifier(ctx, db.AssociateIdentifierParams{Source: source, Namespace: i.Namespace, Value: i.Value, ProfileID: id})
		if err != nil {
			return mutationResult{}, dbError(err)
		}
		if owner != id {
			return mutationResult{}, problem(409, "Identifier conflict", "The source-scoped identifier is already associated with a profile.")
		}
	}
	if len(m.Identifiers) > 0 {
		// New identifiers can change which unified customer the record belongs to.
		if err = q.EnqueueReconciliation(ctx, id); err != nil {
			return mutationResult{}, err
		}
	}
	count, err := q.CountIdentifiers(ctx, id)
	if err != nil {
		return mutationResult{}, err
	}
	if count > 100 {
		return mutationResult{}, problem(422, "Profile limit", "A profile may contain at most 100 identifiers.")
	}
	raw, _ := json.Marshal(attrs)
	if err = q.UpdateProfile(ctx, db.UpdateProfileParams{ID: id, Attributes: raw, Version: next}); err != nil {
		return mutationResult{}, err
	}
	eventType := "profile.updated.v1"
	if create {
		eventType = "profile.created.v1"
	}
	payload, _ := json.Marshal(map[string]any{"profile_id": id, "version": next, "mutation_id": mid, "source": source})
	if err = q.AppendOutbox(ctx, db.AppendOutboxParams{ID: mid, Type: eventType, ProfileID: id, Version: next, Source: source, Payload: payload}); err != nil {
		return mutationResult{}, err
	}
	return mutationResult{Status: status, Body: body, MutationID: mid, Version: next}, nil
}

// Get reads the unified customer a profile belongs to (ADR 0018); any member ID names it. Attributes are
// resolved per attribute from the members' live values under the configured policies.
func (s Store) Get(ctx context.Context, id string) (profiles.Customer, error) {
	c := profiles.Customer{Aliases: []string{}, Attributes: map[string]json.RawMessage{}, ResolvedFrom: map[string]string{},
		Records: []profiles.SourceRecord{}, Identifiers: []profiles.Identifier{}, Provenance: []profiles.Decision{}}
	// A consistent snapshot keeps the members, their values, identifiers and history aligned.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return c, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if _, err = q.RecordCreator(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return c, problem(404, "Not found", "Profile does not exist.")
	} else if err != nil {
		return c, err
	}
	if c.ID, err = q.CanonicalOf(ctx, id); err != nil {
		return c, err
	}
	members, err := q.CustomerMembers(ctx, c.ID)
	if err != nil {
		return c, err
	}
	ids := make([]string, 0, len(members))
	values := map[string]map[string]json.RawMessage{}
	for _, m := range members {
		r := profiles.SourceRecord{ID: m.ID, Source: m.CreatorSource.String, Version: m.Version, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt}
		if err = json.Unmarshal(m.Attributes, &r.Attributes); err != nil {
			return c, err
		}
		values[m.ID] = r.Attributes
		ids = append(ids, m.ID)
		c.Records = append(c.Records, r)
		if m.ID != c.ID {
			c.Aliases = append(c.Aliases, m.ID)
		}
		if c.CreatedAt.IsZero() || m.CreatedAt.Before(c.CreatedAt) {
			c.CreatedAt = m.CreatedAt
		}
		if m.UpdatedAt.After(c.UpdatedAt) {
			c.UpdatedAt = m.UpdatedAt
		}
		identifiers, err := q.ListIdentifiers(ctx, m.ID)
		if err != nil {
			return c, err
		}
		for _, i := range identifiers {
			c.Identifiers = append(c.Identifiers, profiles.Identifier{ProfileID: m.ID, Source: i.Source, Namespace: i.Namespace, Value: i.Value})
		}
		decisions, err := q.ListProvenance(ctx, m.ID)
		if err != nil {
			return c, err
		}
		for _, d := range decisions {
			decision := profiles.Decision{ProfileID: m.ID, Path: d.Path, Source: d.Source, MutationID: d.MutationID, At: d.At, Outcome: d.Outcome, Rule: d.Rule}
			if d.AttributeRevision.Valid {
				decision.AttributeRevision = &d.AttributeRevision.Int32
			}
			c.Provenance = append(c.Provenance, decision)
		}
	}
	policyRows, err := q.ListResolutionPolicies(ctx)
	if err != nil {
		return c, err
	}
	policies := map[string]*profiles.ResolutionPolicy{}
	for _, p := range policyRows {
		policies[p.Path] = &profiles.ResolutionPolicy{Strategy: p.Strategy, SourcePriority: p.SourcePriority}
	}
	contributions, err := q.MemberContributions(ctx, ids)
	if err != nil {
		return c, err
	}
	byPath := map[string][]profiles.Contribution{}
	for _, k := range contributions {
		contribution := profiles.Contribution{Record: k.ProfileID, Source: k.Source, ContributedAt: k.ContributedAt, UpdatedAt: k.UpdatedAt}
		if !k.Tombstone {
			contribution.Value = values[k.ProfileID][k.Path]
		}
		byPath[k.Path] = append(byPath[k.Path], contribution)
	}
	for path, candidates := range byPath {
		if chosen, ok := profiles.Resolve(policies[path], candidates); ok {
			c.Attributes[path], c.ResolvedFrom[path] = chosen.Value, chosen.Record
		}
	}
	return c, tx.Commit(ctx)
}

// Summary is one unified customer in the list: its canonical ID and how many source records it unites.
type Summary struct {
	ID      string `json:"id"`
	Records int64  `json:"records"`
}
type Page struct {
	Items      []Summary `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

// List pages through unified customers by canonical ID.
func (s Store) List(ctx context.Context, after string, limit int) (Page, error) {
	p := Page{Items: []Summary{}}
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := db.New(s.Pool).ListCustomers(ctx, db.ListCustomersParams{AfterID: after, PageSize: int32(limit + 1)})
	if err != nil {
		return p, err
	}
	for _, row := range rows {
		p.Items = append(p.Items, Summary{ID: row.ID, Records: row.Records})
	}
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		p.NextCursor = p.Items[limit-1].ID
	}
	return p, nil
}
