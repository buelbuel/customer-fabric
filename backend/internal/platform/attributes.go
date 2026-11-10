package platform

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5/pgtype"
)

// attributeAdoptionLimit bounds the stored values a new definition checks. Past it the definition
// is refused rather than applied unchecked.
const attributeAdoptionLimit = 10000

// DefineAttribute appends the next revision of an attribute. expected is the revision the operator
// read, 0 for a new attribute, so concurrent operators cannot both revise from the same state. A first
// revision must accept every value already stored under the name, so typing never strands a value.
func (s Store) DefineAttribute(ctx context.Context, d profiles.AttributeDefinition, expected int32) (profiles.AttributeDefinition, error) {
	if err := d.Validate(); err != nil {
		return d, problem(422, "Invalid attribute definition", err.Error())
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockAttributeSchemaExclusive(ctx); err != nil {
		return d, err
	}
	current, err := q.GetCurrentAttributeDefinitions(ctx, []string{d.Path})
	if err != nil {
		return d, err
	}
	d.Revision = 1
	if len(current) == 1 {
		d.Revision = current[0].Revision + 1
	}
	if len(current) == 1 && expected == 0 {
		return d, problem(428, "Precondition required", "This attribute exists; supply its current revision in If-Match to revise it.")
	}
	if d.Revision-1 != expected {
		return d, problem(412, "Stale attribute revision", "Read the attribute's current revision and decide again.")
	}
	if len(current) == 1 {
		if err = d.Supersedes(definitionFromRow(current[0])); err != nil {
			return d, problem(409, "Incompatible attribute revision", err.Error())
		}
	} else if err = adoptStoredValues(ctx, q, d); err != nil {
		return d, err
	}
	params := db.InsertAttributeDefinitionParams{Path: d.Path, Revision: d.Revision, Type: string(d.Type), EnumValues: d.Values}
	if d.MaxLength != nil {
		params.MaxLength = pgtype.Int4{Int32: *d.MaxLength, Valid: true}
	}
	if d.CreatedAt, err = q.InsertAttributeDefinition(ctx, params); err != nil {
		return d, err
	}
	return d, tx.Commit(ctx)
}

func adoptStoredValues(ctx context.Context, q *db.Queries, d profiles.AttributeDefinition) error {
	values, err := q.ListStoredAttributeValues(ctx, db.ListStoredAttributeValuesParams{Path: d.Path, RowLimit: attributeAdoptionLimit + 1})
	if err != nil {
		return err
	}
	if len(values) > attributeAdoptionLimit {
		return problem(409, "Too many stored values", fmt.Sprintf("More than %d profiles hold this attribute, so the definition cannot check them.", attributeAdoptionLimit))
	}
	refused := 0
	for _, v := range values {
		if d.Check(json.RawMessage(v)) != nil {
			refused++
		}
	}
	if refused > 0 {
		return problem(409, "Stored values do not conform", fmt.Sprintf("%d stored values do not match this definition. Their owners must correct or remove them first.", refused))
	}
	return nil
}

// AttributeDefinitions lists the current revision of every defined attribute.
func (s Store) AttributeDefinitions(ctx context.Context) ([]profiles.AttributeDefinition, error) {
	rows, err := db.New(s.Pool).ListCurrentAttributeDefinitions(ctx)
	return definitionsFromRows(rows), err
}

// AttributeRevisions lists every revision of one attribute.
func (s Store) AttributeRevisions(ctx context.Context, path string) ([]profiles.AttributeDefinition, error) {
	rows, err := db.New(s.Pool).ListAttributeRevisions(ctx, path)
	if err == nil && len(rows) == 0 {
		return nil, problem(404, "Not found", "The attribute is not defined.")
	}
	return definitionsFromRows(rows), err
}

// checkTypedValues validates set values against the current definitions, holding the schema lock
// shared until the caller's transaction ends. It returns the revision each typed path was checked under.
func checkTypedValues(ctx context.Context, q *db.Queries, set map[string]json.RawMessage) (map[string]int32, error) {
	if len(set) == 0 {
		return nil, nil
	}
	if err := q.LockAttributeSchemaShared(ctx); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(set))
	for path := range set {
		paths = append(paths, path)
	}
	rows, err := q.GetCurrentAttributeDefinitions(ctx, paths)
	if err != nil {
		return nil, err
	}
	revisions := map[string]int32{}
	for _, row := range rows {
		d := definitionFromRow(row)
		if err = d.Check(set[d.Path]); err != nil {
			return nil, problem(422, "Invalid attribute value", err.Error())
		}
		revisions[d.Path] = d.Revision
	}
	return revisions, nil
}

func definitionFromRow(r db.AttributeDefinition) profiles.AttributeDefinition {
	d := profiles.AttributeDefinition{Path: r.Path, Revision: r.Revision, Type: profiles.AttributeType(r.Type), Values: r.EnumValues, CreatedAt: r.CreatedAt}
	if r.MaxLength.Valid {
		d.MaxLength = &r.MaxLength.Int32
	}
	return d
}

func definitionsFromRows(rows []db.AttributeDefinition) []profiles.AttributeDefinition {
	out := make([]profiles.AttributeDefinition, 0, len(rows))
	for _, r := range rows {
		out = append(out, definitionFromRow(r))
	}
	return out
}
