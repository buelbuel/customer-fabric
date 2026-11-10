package platform

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

// customerViewRecordLimit bounds one view. A customer past it is refused rather than truncated,
// because a partial view would look complete.
const customerViewRecordLimit = 50

// CustomerView reads a customer's verified source records for one reader in one read-only snapshot,
// together with the attribute governance it applies. It computes the view on every call and writes
// nothing, so a revocation or a narrowed policy applies to the next read.
func (s Store) CustomerView(ctx context.Context, anchorID string, reader profiles.Reader) (profiles.CustomerView, error) {
	var view profiles.CustomerView
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return view, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	kratos, err := q.GetCustomerLinkForProfile(ctx, anchorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return view, problem(404, "Not found", "The profile is not a customer anchor.")
	}
	if err != nil {
		return view, err
	}
	return customerViewIn(ctx, q, anchorID, kratos, reader)
}

// customerViewIn builds a view through q, so the caller's transaction decides the snapshot.
func customerViewIn(ctx context.Context, q *db.Queries, anchorID, kratos string, reader profiles.Reader) (profiles.CustomerView, error) {
	var view profiles.CustomerView
	subject, err := q.GetCustomerSubjectByAnchor(ctx, mustUUID(anchorID))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return view, err
	}
	// Even association metadata would show which sources an erased customer was linked to.
	if err == nil && subject.LifecycleState != "active" {
		return view, problem(409, "Customer unavailable", "The customer's lifecycle state does not permit this view.")
	}
	rows, err := q.ListCustomerViewRecords(ctx, db.ListCustomerViewRecordsParams{CustomerSubject: kratos, RowLimit: customerViewRecordLimit + 1})
	if err != nil {
		return view, err
	}
	if len(rows) > customerViewRecordLimit {
		return view, problem(409, "View too large", "The customer has more linked records than one view can present.")
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.RecordProfileID)
	}
	evidence, err := q.ListCustomerViewEvidence(ctx, ids)
	if err != nil {
		return view, err
	}
	byProfile := map[string]map[string]profiles.AttributeEvidence{}
	for _, e := range evidence {
		if byProfile[e.ProfileID] == nil {
			byProfile[e.ProfileID] = map[string]profiles.AttributeEvidence{}
		}
		write := profiles.AttributeEvidence{Owner: e.Owner, MutationID: e.MutationID, At: e.At}
		if e.AttributeRevision.Valid {
			write.AttributeRevision = &e.AttributeRevision.Int32
		}
		byProfile[e.ProfileID][e.Path] = write
	}
	records := make([]profiles.LinkedRecord, 0, len(rows))
	paths := []string{}
	for _, r := range rows {
		var attributes map[string]json.RawMessage
		if err = json.Unmarshal(r.Attributes, &attributes); err != nil {
			return view, err
		}
		for path := range attributes {
			paths = append(paths, path)
		}
		records = append(records, profiles.LinkedRecord{AssociationID: r.AssociationID, Source: r.Source, LinkedAt: r.LinkedAt,
			RecordProfileID: r.RecordProfileID, Version: r.Version, UpdatedAt: r.UpdatedAt, SourceOwned: r.SourceOwned,
			Identifiers: r.IdentifierCount, Restricted: r.RecordRestricted,
			Attributes: attributes, Evidence: byProfile[r.RecordProfileID]})
	}
	governance, err := loadGovernance(ctx, q, paths)
	if err != nil {
		return view, err
	}
	return profiles.BuildCustomerView(anchorID, reader, records, governance), nil
}
