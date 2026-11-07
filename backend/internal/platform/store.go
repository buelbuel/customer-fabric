package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct{ Pool *pgxpool.Pool }
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
	var status int
	var body []byte
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
	create := id == ""
	attrs := map[string]json.RawMessage{}
	owners := map[string]string{}
	current := int64(0)
	if create {
		id = UUID()
		if err = q.CreateProfile(ctx, id); err != nil {
			return 0, nil, err
		}
	} else {
		locked, err := q.LockProfile(ctx, id)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, problem(404, "Not found", "Profile does not exist.")
		}
		if err != nil {
			return 0, nil, err
		}
		current = locked.Version
		if current != version {
			return 0, nil, problem(412, "Version conflict", "Fetch the profile and retry with its current ETag and a new key.")
		}
		if err = json.Unmarshal(locked.Attributes, &attrs); err != nil {
			return 0, nil, err
		}
		ownership, err := q.ListOwnership(ctx, id)
		if err != nil {
			return 0, nil, err
		}
		for _, item := range ownership {
			owners[item.Path] = item.Source
		}
	}
	mid := UUID()
	conflicts := profiles.CheckOwnership(m, owners, source)
	status = 200
	if create {
		status = 201
	}
	next := current + 1
	outcome := "accepted"
	if len(conflicts) > 0 {
		status = 409
		outcome = "rejected"
		body, _ = json.Marshal(problem(409, "Ownership conflict", "Another source owns one or more requested attributes. No changes were applied."))
	} else {
		body, _ = json.Marshal(profiles.Receipt{ID: id, Version: next, MutationID: mid})
	}
	if err = q.RecordMutation(ctx, db.RecordMutationParams{ID: mid, Source: source, Key: key, RequestHash: hash, ProfileID: id, Status: int32(status), Result: body}); err != nil {
		return 0, nil, err
	}
	paths := m.Paths()
	if outcome == "rejected" {
		paths = conflicts
	}
	for _, path := range paths {
		if err = q.RecordProvenance(ctx, db.RecordProvenanceParams{ProfileID: id, Path: path, Source: source, MutationID: mid, Outcome: outcome}); err != nil {
			return 0, nil, err
		}
	}
	if outcome == "accepted" {
		for k, v := range m.Set {
			attrs[k] = v
		}
		for _, k := range m.Remove {
			delete(attrs, k)
		}
		if len(attrs) > 100 {
			return 0, nil, problem(422, "Profile limit", "A profile may contain at most 100 attributes.")
		}
		for _, path := range paths {
			_, present := attrs[path]
			if err = q.SetOwnership(ctx, db.SetOwnershipParams{ProfileID: id, Path: path, Source: source, Tombstone: !present}); err != nil {
				return 0, nil, err
			}
		}
		for _, i := range m.Identifiers {
			// The unique constraint prevents concurrent claims, including claims by different profiles.
			owner, err := q.AssociateIdentifier(ctx, db.AssociateIdentifierParams{Source: source, Namespace: i.Namespace, Value: i.Value, ProfileID: id})
			if err != nil {
				return 0, nil, dbError(err)
			}
			if owner != id {
				return 0, nil, problem(409, "Identifier conflict", "The source-scoped identifier is already associated with a profile.")
			}
		}
		count, err := q.CountIdentifiers(ctx, id)
		if err != nil {
			return 0, nil, err
		}
		if count > 100 {
			return 0, nil, problem(422, "Profile limit", "A profile may contain at most 100 identifiers.")
		}
		raw, _ := json.Marshal(attrs)
		if err = q.UpdateProfile(ctx, db.UpdateProfileParams{ID: id, Attributes: raw, Version: next}); err != nil {
			return 0, nil, err
		}
		eventType := "profile.updated.v1"
		if create {
			eventType = "profile.created.v1"
		}
		payload, _ := json.Marshal(map[string]any{"profile_id": id, "version": next, "mutation_id": mid, "source": source})
		if err = q.AppendOutbox(ctx, db.AppendOutboxParams{ID: mid, Type: eventType, ProfileID: id, Version: next, Source: source, Payload: payload}); err != nil {
			return 0, nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return 0, nil, err
	}
	return status, body, nil
}

func (s Store) Get(ctx context.Context, id string) (profiles.Profile, error) {
	p := profiles.Profile{Identifiers: []profiles.Identifier{}, Provenance: []profiles.Decision{}}
	// A consistent snapshot keeps the version, attributes, identifiers and history aligned.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return p, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := q.GetProfile(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, problem(404, "Not found", "Profile does not exist.")
	}
	if err != nil {
		return p, err
	}
	p.ID, p.Version, p.CreatedAt, p.UpdatedAt = row.ID, row.Version, row.CreatedAt, row.UpdatedAt
	if err = json.Unmarshal(row.Attributes, &p.Attributes); err != nil {
		return p, err
	}
	identifiers, err := q.ListIdentifiers(ctx, id)
	if err != nil {
		return p, err
	}
	for _, i := range identifiers {
		p.Identifiers = append(p.Identifiers, profiles.Identifier{Source: i.Source, Namespace: i.Namespace, Value: i.Value})
	}
	decisions, err := q.ListProvenance(ctx, id)
	if err != nil {
		return p, err
	}
	for _, d := range decisions {
		p.Provenance = append(p.Provenance, profiles.Decision{Path: d.Path, Source: d.Source, MutationID: d.MutationID, At: d.At, Outcome: d.Outcome, Rule: d.Rule})
	}
	return p, tx.Commit(ctx)
}

type Summary struct {
	ID      string `json:"id"`
	Version int64  `json:"version"`
}
type Page struct {
	Items      []Summary `json:"items"`
	NextCursor string    `json:"next_cursor,omitempty"`
}

func (s Store) List(ctx context.Context, after string, limit int) (Page, error) {
	p := Page{Items: []Summary{}}
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := db.New(s.Pool).ListProfiles(ctx, db.ListProfilesParams{AfterID: after, PageSize: int32(limit + 1)})
	if err != nil {
		return p, err
	}
	for _, row := range rows {
		p.Items = append(p.Items, Summary{ID: row.ID, Version: row.Version})
	}
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		p.NextCursor = p.Items[limit-1].ID
	}
	return p, nil
}
