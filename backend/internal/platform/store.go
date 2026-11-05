package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

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
	if _, err = tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, source+":"+key); err != nil {
		return 0, nil, err
	}
	var oldHash string
	var status int
	var body []byte
	err = tx.QueryRow(ctx, `SELECT request_hash,status,result FROM mutations WHERE source=$1 AND key=$2`, source, key).Scan(&oldHash, &status, &body)
	if err == nil {
		if oldHash != hash {
			return 0, nil, problem(409, "Idempotency conflict", "Use a new key for a different request.")
		}
		return status, body, nil
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
		if _, err = tx.Exec(ctx, `INSERT INTO profiles(id,version,attributes) VALUES($1,1,'{}')`, id); err != nil {
			return 0, nil, err
		}
	} else {
		var raw []byte
		err = tx.QueryRow(ctx, `SELECT version,attributes FROM profiles WHERE id=$1 FOR UPDATE`, id).Scan(&current, &raw)
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, nil, problem(404, "Not found", "Profile does not exist.")
		}
		if err != nil {
			return 0, nil, err
		}
		if current != version {
			return 0, nil, problem(412, "Version conflict", "Fetch the profile and retry with its current ETag and a new key.")
		}
		if err = json.Unmarshal(raw, &attrs); err != nil {
			return 0, nil, err
		}
		rows, e := tx.Query(ctx, `SELECT path,source FROM attribute_ownership WHERE profile_id=$1`, id)
		if e != nil {
			return 0, nil, e
		}
		for rows.Next() {
			var p, o string
			if e = rows.Scan(&p, &o); e != nil {
				rows.Close()
				return 0, nil, e
			}
			owners[p] = o
		}
		e = rows.Err()
		rows.Close()
		if e != nil {
			return 0, nil, e
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
	if _, err = tx.Exec(ctx, `INSERT INTO mutations(id,source,key,request_hash,profile_id,status,result) VALUES($1,$2,$3,$4,$5,$6,$7)`, mid, source, key, hash, id, status, body); err != nil {
		return 0, nil, err
	}
	paths := m.Paths()
	if outcome == "rejected" {
		paths = conflicts
	}
	for _, path := range paths {
		if _, err = tx.Exec(ctx, `INSERT INTO provenance(profile_id,path,source,mutation_id,outcome,rule) VALUES($1,$2,$3,$4,$5,'first-writer-owns')`, id, path, source, mid, outcome); err != nil {
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
			if _, err = tx.Exec(ctx, `INSERT INTO attribute_ownership(profile_id,path,source,tombstone) VALUES($1,$2,$3,$4) ON CONFLICT(profile_id,path) DO UPDATE SET tombstone=EXCLUDED.tombstone`, id, path, source, !present); err != nil {
				return 0, nil, err
			}
		}
		for _, i := range m.Identifiers {
			// The unique constraint prevents concurrent claims, including claims by different profiles.
			var owner string
			err = tx.QueryRow(ctx, `INSERT INTO external_identifiers(source,namespace,value,profile_id) VALUES($1,$2,$3,$4) ON CONFLICT(source,namespace,value) DO UPDATE SET value=EXCLUDED.value RETURNING profile_id::text`, source, i.Namespace, i.Value, id).Scan(&owner)
			if err != nil {
				return 0, nil, dbError(err)
			}
			if owner != id {
				return 0, nil, problem(409, "Identifier conflict", "The source-scoped identifier is already associated with a profile.")
			}
		}
		var count int
		if err = tx.QueryRow(ctx, `SELECT count(*) FROM external_identifiers WHERE profile_id=$1`, id).Scan(&count); err != nil {
			return 0, nil, err
		}
		if count > 100 {
			return 0, nil, problem(422, "Profile limit", "A profile may contain at most 100 identifiers.")
		}
		raw, _ := json.Marshal(attrs)
		if _, err = tx.Exec(ctx, `UPDATE profiles SET attributes=$2,version=$3,updated_at=now() WHERE id=$1`, id, raw, next); err != nil {
			return 0, nil, err
		}
		eventType := "profile.updated.v1"
		if create {
			eventType = "profile.created.v1"
		}
		payload, _ := json.Marshal(map[string]any{"profile_id": id, "version": next, "mutation_id": mid, "source": source})
		if _, err = tx.Exec(ctx, `INSERT INTO outbox(id,type,profile_id,version,source,payload) VALUES($1,$2,$3,$4,$5,$6)`, mid, eventType, id, next, source, payload); err != nil {
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
	var raw []byte
	err = tx.QueryRow(ctx, `SELECT id::text,version,attributes,created_at,updated_at FROM profiles WHERE id=$1`, id).Scan(&p.ID, &p.Version, &raw, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, problem(404, "Not found", "Profile does not exist.")
	}
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal(raw, &p.Attributes); err != nil {
		return p, err
	}
	rows, err := tx.Query(ctx, `SELECT source,namespace,value FROM external_identifiers WHERE profile_id=$1 ORDER BY source,namespace,value`, id)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var i profiles.Identifier
		if err = rows.Scan(&i.Source, &i.Namespace, &i.Value); err != nil {
			rows.Close()
			return p, err
		}
		p.Identifiers = append(p.Identifiers, i)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
	}
	rows, err = tx.Query(ctx, `SELECT path,source,mutation_id::text,at,outcome,rule FROM provenance WHERE profile_id=$1 ORDER BY sequence DESC LIMIT 100`, id)
	if err != nil {
		return p, err
	}
	for rows.Next() {
		var d profiles.Decision
		if err = rows.Scan(&d.Path, &d.Source, &d.MutationID, &d.At, &d.Outcome, &d.Rule); err != nil {
			rows.Close()
			return p, err
		}
		p.Provenance = append(p.Provenance, d)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return p, err
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
	rows, err := s.Pool.Query(ctx, `SELECT id::text,version FROM profiles WHERE id>$1::uuid ORDER BY id LIMIT $2`, after, limit+1)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var i Summary
		if err = rows.Scan(&i.ID, &i.Version); err != nil {
			return p, err
		}
		p.Items = append(p.Items, i)
	}
	if len(p.Items) > limit {
		p.Items = p.Items[:limit]
		p.NextCursor = p.Items[limit-1].ID
	}
	return p, rows.Err()
}
