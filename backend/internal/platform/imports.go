package platform

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

const (
	importBatchRows  = 1000
	importLineBytes  = 64 << 10
	importQuarantine = 1000
)

// Import is a source import's progress. Checkpoint is the last committed line; the next batch starts
// at checkpoint+1.
type Import struct {
	ID          string           `json:"id"`
	State       string           `json:"state"`
	Checkpoint  int64            `json:"checkpoint"`
	Applied     int64            `json:"applied"`
	Created     int64            `json:"created"`
	Quarantined int64            `json:"quarantined"`
	MaxErrors   int32            `json:"max_errors"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	Quarantine  []QuarantinedRow `json:"quarantine"`
}

type QuarantinedRow struct {
	Line   int64  `json:"line"`
	Reason string `json:"reason"`
}

func importFromRow(r db.SourceImport) Import {
	return Import{ID: r.ID, State: r.State, Checkpoint: r.Checkpoint, Applied: r.Applied, Created: r.Created, Quarantined: r.Quarantined,
		MaxErrors: r.MaxErrors, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Quarantine: []QuarantinedRow{}}
}

func (s Store) CreateImport(ctx context.Context, source string, maxErrors int32) (Import, error) {
	row, err := db.New(s.Pool).InsertImport(ctx, db.InsertImportParams{ID: UUID(), Source: source, MaxErrors: maxErrors})
	return importFromRow(row), err
}

// GetImport returns progress and the first quarantined lines, for the owning source only.
func (s Store) GetImport(ctx context.Context, source, id string) (Import, error) {
	q := db.New(s.Pool)
	row, err := q.GetImport(ctx, db.GetImportParams{ID: id, Source: source})
	if errors.Is(err, pgx.ErrNoRows) {
		return Import{}, problem(404, "Not found", "Unknown import.")
	}
	if err != nil {
		return Import{}, err
	}
	imp := importFromRow(row)
	quarantine, err := q.ListImportQuarantine(ctx, db.ListImportQuarantineParams{ImportID: id, RowLimit: importQuarantine})
	for _, r := range quarantine {
		imp.Quarantine = append(imp.Quarantine, QuarantinedRow{Line: r.Line, Reason: r.Reason})
	}
	return imp, err
}

// CompleteImport closes an open import; later batches are refused.
func (s Store) CompleteImport(ctx context.Context, source, id string) (Import, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Import{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := lockOpenImport(ctx, q, source, id)
	if err != nil {
		return Import{}, err
	}
	if err = q.AdvanceImport(ctx, db.AdvanceImportParams{ID: id, Checkpoint: row.Checkpoint, State: "completed"}); err != nil {
		return Import{}, err
	}
	if err = tx.Commit(ctx); err != nil {
		return Import{}, err
	}
	return s.GetImport(ctx, source, id)
}

func lockOpenImport(ctx context.Context, q *db.Queries, source, id string) (db.SourceImport, error) {
	row, err := q.LockImport(ctx, db.LockImportParams{ID: id, Source: source})
	if errors.Is(err, pgx.ErrNoRows) {
		return row, problem(404, "Not found", "Unknown import.")
	}
	if err == nil && row.State != "open" {
		return row, problem(409, "Import closed", fmt.Sprintf("The import is %s and accepts no more rows.", row.State))
	}
	return row, err
}

// ImportBatch applies NDJSON rows starting at line offset, which must be the import's checkpoint+1.
// Each row commits with the checkpoint in its own transaction, so a client that loses a response reads
// the checkpoint and resends from there; nothing is applied twice and nothing is skipped. Invalid rows
// are quarantined by line and reason and count against max_errors; past it the import fails.
func (s Store) ImportBatch(ctx context.Context, source, id string, offset int64, body io.Reader) (Import, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 4096), importLineBytes)
	line := offset
	for rows := 0; scanner.Scan(); rows++ {
		if rows == importBatchRows {
			return Import{}, problem(413, "Batch too large", fmt.Sprintf("Send at most %d rows per batch.", importBatchRows))
		}
		failed, err := s.importRow(ctx, source, id, line, scanner.Bytes())
		if err != nil {
			return Import{}, err
		}
		if failed {
			break
		}
		line++
	}
	var tooLarge *http.MaxBytesError
	switch err := scanner.Err(); {
	case errors.Is(err, bufio.ErrTooLong):
		return Import{}, problem(413, "Line too long", fmt.Sprintf("Line %d exceeds %d bytes; rows before it were committed.", line, importLineBytes))
	case errors.As(err, &tooLarge):
		return Import{}, problem(413, "Batch too large", fmt.Sprintf("The batch body exceeds %d bytes at line %d; rows before it were committed.", tooLarge.Limit, line))
	case err != nil:
		return Import{}, err
	}
	return s.GetImport(ctx, source, id)
}

// importRow commits one row with the checkpoint and reports whether the import has now failed.
func (s Store) importRow(ctx context.Context, source, id string, line int64, raw []byte) (bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	imp, err := lockOpenImport(ctx, q, source, id)
	if err != nil {
		return false, err
	}
	if line != imp.Checkpoint+1 {
		return false, problem(409, "Out of order batch", fmt.Sprintf("Resume this import at line %d.", imp.Checkpoint+1))
	}
	advance := db.AdvanceImportParams{ID: id, Checkpoint: line, State: "open"}
	reason, created, err := applyImportRow(ctx, tx, source, id, line, raw)
	if err != nil {
		return false, err
	}
	if reason != "" {
		if err = q.QuarantineImportRow(ctx, db.QuarantineImportRowParams{ImportID: id, Line: line, Reason: reason}); err != nil {
			return false, err
		}
		advance.Quarantined = 1
		if imp.Quarantined+1 > int64(imp.MaxErrors) {
			advance.State = "failed"
		}
	} else if created {
		advance.Created = 1
	} else {
		advance.Applied = 1
	}
	if err = q.AdvanceImport(ctx, advance); err != nil {
		return false, err
	}
	return advance.State == "failed", tx.Commit(ctx)
}

// applyImportRow writes one valid row through the governed write path inside a savepoint, so a refused
// write leaves only its quarantine entry. It returns a quarantine reason or whether a record was created.
func applyImportRow(ctx context.Context, tx pgx.Tx, source, id string, line int64, raw []byte) (string, bool, error) {
	row, reason := ingest.ParseImportRow(raw, duplicateKeys)
	if reason != "" {
		return reason, false, nil
	}
	savepoint, err := tx.Begin(ctx)
	if err != nil {
		return "", false, err
	}
	defer savepoint.Rollback(ctx)
	q := db.New(savepoint).WithTx(savepoint)
	// Rows never reuse a client key: the colon is outside the Idempotency-Key alphabet.
	key := fmt.Sprintf("import:%s:%d", id, line)
	digest := sha256.Sum256(raw)
	if err = q.LockMutationKey(ctx, source+":"+key); err != nil {
		return "", false, err
	}
	profileID := ""
	record, err := lockSourceRecord(ctx, q, source, row.Record)
	switch {
	case err == nil:
		profileID = record.ProfileID
		// A historical row must not overwrite what a newer source event already decided.
		if _, err = q.GetSourceRecordWatermark(ctx, record.RecordEpoch); err == nil {
			return "newer_events_exist", false, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return "", false, err
		}
	case errors.Is(err, unknownRecord):
		if len(row.Remove) > 0 {
			return ingest.ReasonInvalidRow, false, nil
		}
	default:
		return "", false, err
	}
	create := profileID == ""
	result, err := applyMutation(ctx, q, source, key, hex.EncodeToString(digest[:]), profileID, nil, row.Mutation(create))
	if err != nil {
		return refusal(err)
	}
	if result.Rejected {
		// Keep the rejected mutation and its provenance, as the direct write path does.
		return "ownership_conflict", false, savepoint.Commit(ctx)
	}
	return "", create, savepoint.Commit(ctx)
}

// refusal turns a refused write into a quarantine reason; any other error stops the batch.
func refusal(err error) (string, bool, error) {
	var p *Problem
	if !errors.As(err, &p) {
		return "", false, err
	}
	reason := strings.ToLower(strings.ReplaceAll(p.Title, " ", "_"))
	switch reason {
	case "invalid_attribute_value", "identifier_conflict", "profile_not_writable", "profile_limit", "not_found":
		return reason, false, nil
	}
	return "write_refused", false, nil
}
