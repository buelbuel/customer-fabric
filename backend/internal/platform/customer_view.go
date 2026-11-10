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

// CustomerView reads a customer's verified source records in one read-only snapshot. It computes the
// view on every call and writes nothing, so a revocation applies to the next read.
func (s Store) CustomerView(ctx context.Context, anchorID string, visible map[string][]string) (profiles.CustomerView, error) {
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
		byProfile[e.ProfileID][e.Path] = profiles.AttributeEvidence{Owner: e.Owner, MutationID: e.MutationID, At: e.At}
	}
	records := make([]profiles.LinkedRecord, 0, len(rows))
	for _, r := range rows {
		var attributes map[string]json.RawMessage
		if err = json.Unmarshal(r.Attributes, &attributes); err != nil {
			return view, err
		}
		records = append(records, profiles.LinkedRecord{AssociationID: r.AssociationID, Source: r.Source, LinkedAt: r.LinkedAt,
			RecordProfileID: r.RecordProfileID, Version: r.Version, UpdatedAt: r.UpdatedAt, SourceOwned: r.SourceOwned,
			Identifiers: r.IdentifierCount, Restricted: r.RecordRestricted,
			Attributes: attributes, Evidence: byProfile[r.RecordProfileID]})
	}
	return profiles.BuildCustomerView(anchorID, records, visible), nil
}
