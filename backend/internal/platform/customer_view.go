package platform

import (
	"context"
	"encoding/json"
	"errors"
	"slices"

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
	views, err := customerViewsIn(ctx, q, []customerAnchor{{ProfileID: anchorID, Kratos: kratos}}, reader, nil)
	if err != nil {
		return profiles.CustomerView{}, err
	}
	if views[0].Refused != nil {
		return profiles.CustomerView{}, views[0].Refused
	}
	return views[0].View, nil
}

// customerAnchor is a customer's anchor profile and their Kratos identity.
type customerAnchor struct{ ProfileID, Kratos string }

// customerViewResult is one customer's view, or the refusal that withholds it.
type customerViewResult struct {
	View    profiles.CustomerView
	Refused *Problem
}

// customerViewsIn builds many customers' views through q with a fixed number of reads, in the order of
// anchors. Each view is exactly what a one-customer read decides, including its refusals. A non-nil only
// limits the views to those attribute paths, for callers such as segment rules that read no others;
// whether a view is available does not depend on its attributes.
func customerViewsIn(ctx context.Context, q *db.Queries, anchors []customerAnchor, reader profiles.Reader, only []string) ([]customerViewResult, error) {
	ids, kratos := make([]string, len(anchors)), make([]string, len(anchors))
	for i, a := range anchors {
		ids[i], kratos[i] = a.ProfileID, a.Kratos
	}
	subjects, err := q.ListCustomerSubjectsByAnchors(ctx, ids)
	if err != nil {
		return nil, err
	}
	inactive := map[string]bool{}
	for _, s := range subjects {
		inactive[s.AnchorProfileID] = s.LifecycleState != "active"
	}
	rows, err := q.ListCustomerViewRecordsFor(ctx, db.ListCustomerViewRecordsForParams{CustomerSubjects: kratos, RowLimit: customerViewRecordLimit + 1})
	if err != nil {
		return nil, err
	}
	byCustomer := map[string][]db.ListCustomerViewRecordsForRow{}
	recordIDs := make([]string, 0, len(rows))
	for _, r := range rows {
		byCustomer[r.CustomerSubject] = append(byCustomer[r.CustomerSubject], r)
		recordIDs = append(recordIDs, r.RecordProfileID)
	}
	evidence, err := q.ListCustomerViewEvidence(ctx, db.ListCustomerViewEvidenceParams{ProfileIds: recordIDs, AllPaths: only == nil, Paths: only})
	if err != nil {
		return nil, err
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
	records := make(map[string][]profiles.LinkedRecord, len(byCustomer))
	paths, seen := []string{}, map[string]bool{}
	for customer, list := range byCustomer {
		for _, r := range list {
			var attributes map[string]json.RawMessage
			if err = json.Unmarshal(r.Attributes, &attributes); err != nil {
				return nil, err
			}
			for path := range attributes {
				if only != nil && !slices.Contains(only, path) {
					delete(attributes, path)
					continue
				}
				if !seen[path] {
					seen[path] = true
					paths = append(paths, path)
				}
			}
			records[customer] = append(records[customer], profiles.LinkedRecord{AssociationID: r.AssociationID, Source: r.Source, LinkedAt: r.LinkedAt,
				RecordProfileID: r.RecordProfileID, Version: r.Version, UpdatedAt: r.UpdatedAt, SourceOwned: r.SourceOwned,
				Identifiers: r.IdentifierCount, Restricted: r.RecordRestricted,
				Attributes: attributes, Evidence: byProfile[r.RecordProfileID]})
		}
	}
	governance, err := loadGovernance(ctx, q, paths)
	if err != nil {
		return nil, err
	}
	out := make([]customerViewResult, len(anchors))
	for i, a := range anchors {
		switch {
		// Even association metadata would show which sources an erased customer was linked to.
		case inactive[a.ProfileID]:
			out[i].Refused = problem(409, "Customer unavailable", "The customer's lifecycle state does not permit this view.")
		case len(records[a.Kratos]) > customerViewRecordLimit:
			out[i].Refused = problem(409, "View too large", "The customer has more linked records than one view can present.")
		default:
			out[i].View = profiles.BuildCustomerView(a.ProfileID, reader, records[a.Kratos], governance)
		}
	}
	return out, nil
}
