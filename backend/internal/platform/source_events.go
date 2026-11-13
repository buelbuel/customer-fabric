package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"

	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Event outcomes. Each is durable and final for its event ID, so a retry returns the same answer.
const (
	eventApplied = "applied"
	eventStale   = "stale"
)

// SourceEventReceipt never says whether the record is linked to a customer.
type SourceEventReceipt struct {
	EventID        string `json:"event_id"`
	Outcome        string `json:"outcome"`
	ProfileID      string `json:"profile_id"`
	ProfileVersion *int64 `json:"profile_version,omitempty"`
	MutationID     string `json:"mutation_id,omitempty"`
	Created        bool   `json:"-"`
}

var unknownRecord = problem(404, "Unknown record", "This source has no such record.")

// IngestSourceEvent decides one event in a single transaction: its receipt, the record's ordering
// watermark and any profile, provenance and outbox writes commit together. The event only ever
// reaches the profile bound to the source's own record. A verified association to a customer is
// neither followed nor changed, because it transfers no ownership (ADR 0007).
func (s Store) IngestSourceEvent(ctx context.Context, source string, e ingest.Event) (SourceEventReceipt, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return SourceEventReceipt{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockMutationKey(ctx, "source-event:"+source+":"+e.EventID); err != nil {
		return SourceEventReceipt{}, err
	}
	digest := e.Digest()
	previous, err := q.GetSourceEvent(ctx, db.GetSourceEventParams{Source: source, EventID: e.EventID})
	if err == nil {
		if previous.Digest != digest {
			return SourceEventReceipt{}, problem(409, "Event ID conflict", "This event ID was already used for different content.")
		}
		return storedReceipt(e.EventID, previous), nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return SourceEventReceipt{}, err
	}
	if err = q.LockSourceEpochShared(ctx, source); err != nil {
		return SourceEventReceipt{}, err
	}
	epoch, err := q.GetSourceEpoch(ctx, source)
	if err != nil {
		return SourceEventReceipt{}, err
	}
	if e.SourceEpoch != epoch {
		return SourceEventReceipt{}, problem(409, "Source epoch mismatch", "Send this event with the source's current epoch.")
	}
	record, err := lockSourceRecord(ctx, q, source, e.Record)
	if err != nil {
		return SourceEventReceipt{}, err
	}
	if _, err = lockWritableProfile(ctx, q, record.ProfileID); err != nil {
		return SourceEventReceipt{}, err
	}
	// Ordering is per record but attributes are per profile, so two event-ordered records on one source
	// profile would have incomparable positions over the same values. Its other identifiers (aliases,
	// trusted identity keys) are no such conflict. The profile lock serializes this check with every event.
	others, err := q.OtherOrderedRecords(ctx, db.OtherOrderedRecordsParams{ProfileID: record.ProfileID, RecordEpoch: record.RecordEpoch})
	if err != nil {
		return SourceEventReceipt{}, err
	}
	if others != 0 {
		return SourceEventReceipt{}, problem(409, "Ambiguous record", "Another record of the profile already has an event order, so their events cannot be ordered together.")
	}
	position := ingest.Position{SourceEpoch: e.SourceEpoch, Sequence: e.Sequence}
	watermark, err := q.GetSourceRecordWatermark(ctx, record.RecordEpoch)
	seen := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return SourceEventReceipt{}, err
	}
	receipt := SourceEventReceipt{EventID: e.EventID, Outcome: eventStale, ProfileID: record.ProfileID, Created: true}
	if !seen || position.After(ingest.Position{SourceEpoch: watermark.SourceEpoch, Sequence: watermark.Sequence}) {
		// A client Idempotency-Key cannot contain a colon, so this key space is reserved for events.
		eventKey := sha256.Sum256([]byte(e.EventID))
		result, err := applyMutation(ctx, q, source, "source-event:"+hex.EncodeToString(eventKey[:]), digest, record.ProfileID, nil, e.Mutation())
		if err != nil {
			return SourceEventReceipt{}, err
		}
		receipt.MutationID = result.MutationID
		receipt.Outcome, receipt.ProfileVersion = eventApplied, &result.Version
		if err = q.SetSourceRecordWatermark(ctx, db.SetSourceRecordWatermarkParams{RecordEpoch: record.RecordEpoch, SourceEpoch: e.SourceEpoch, Sequence: e.Sequence}); err != nil {
			return SourceEventReceipt{}, err
		}
	}
	params := db.InsertSourceEventParams{
		Source: source, EventID: e.EventID, Digest: digest, RecordEpoch: record.RecordEpoch, ProfileID: record.ProfileID,
		SourceEpoch: e.SourceEpoch, Sequence: e.Sequence, OccurredAt: e.OccurredAt, Outcome: receipt.Outcome,
	}
	if receipt.MutationID != "" {
		params.MutationID = mustUUID(receipt.MutationID)
	}
	if receipt.ProfileVersion != nil {
		params.ProfileVersion = pgtype.Int8{Int64: *receipt.ProfileVersion, Valid: true}
	}
	if err = q.InsertSourceEvent(ctx, params); err != nil {
		return SourceEventReceipt{}, err
	}
	return receipt, tx.Commit(ctx)
}

// lockSourceRecord takes the record-lifetime lock shared with link, unlink and record evidence, and
// resolves again under it, so an identifier recreated meanwhile cannot receive the event.
func lockSourceRecord(ctx context.Context, q *db.Queries, source string, r ingest.Record) (db.ResolveSourceRecordRow, error) {
	params := db.ResolveSourceRecordParams{Source: source, Namespace: r.Namespace, Value: r.Value}
	before, err := q.ResolveSourceRecord(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) {
		return before, unknownRecord
	}
	if err != nil {
		return before, err
	}
	if err = q.LockLinkRecord(ctx, before.RecordEpoch); err != nil {
		return before, err
	}
	locked, err := q.ResolveSourceRecord(ctx, params)
	if errors.Is(err, pgx.ErrNoRows) || (err == nil && locked.RecordEpoch != before.RecordEpoch) {
		return locked, unknownRecord
	}
	return locked, err
}

func storedReceipt(eventID string, row db.GetSourceEventRow) SourceEventReceipt {
	receipt := SourceEventReceipt{EventID: eventID, Outcome: row.Outcome, ProfileID: row.ProfileID}
	if row.MutationID.Valid {
		receipt.MutationID = row.MutationID.String()
	}
	if row.ProfileVersion.Valid {
		receipt.ProfileVersion = &row.ProfileVersion.Int64
	}
	return receipt
}

// SourceEpoch returns a source's current epoch.
func (s Store) SourceEpoch(ctx context.Context, source string) (int64, error) {
	epoch, err := db.New(s.Pool).GetSourceEpoch(ctx, source)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, problem(404, "Not found", "Unknown source.")
	}
	return epoch, err
}

// AdvanceSourceEpoch moves a source on from the epoch the operator read, so a retried request
// cannot advance it twice. It waits for in-flight events, and later events from the old epoch are
// refused. Ordering restarts on each record under the new epoch.
func (s Store) AdvanceSourceEpoch(ctx context.Context, source string, expected int64) (int64, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if err = q.LockSourceEpochExclusive(ctx, source); err != nil {
		return 0, err
	}
	epoch, err := q.AdvanceSourceEpoch(ctx, db.AdvanceSourceEpochParams{Name: source, Epoch: expected})
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = q.GetSourceEpoch(ctx, source); errors.Is(err, pgx.ErrNoRows) {
			return 0, problem(404, "Not found", "Unknown source.")
		} else if err != nil {
			return 0, err
		}
		return 0, problem(412, "Stale source epoch", "Read the source's current epoch and decide again.")
	}
	if err != nil {
		return 0, err
	}
	return epoch, tx.Commit(ctx)
}
