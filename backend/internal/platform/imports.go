package platform

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/ingest"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
)

const (
	importBatchRows  = 1000
	importLineBytes  = 64 << 10
	importQuarantine = 1000
	// importChunkRows rows commit together with the checkpoint, amortizing commit flushes; resume stays
	// exact because a chunk's rows and checkpoint commit at once. A chunk holds its row locks until it
	// commits, so concurrent imports of the same records can deadlock; the losing chunk rolls back whole
	// and its batch can be resent from the checkpoint.
	importChunkRows = 100
)

// Import is a source import's progress. Checkpoint is the last committed line; the next batch starts
// at checkpoint+1.
type Import struct {
	ID          string             `json:"id"`
	Format      string             `json:"format"`
	Mapping     *ingest.CSVMapping `json:"mapping,omitempty"`
	State       string             `json:"state"`
	Checkpoint  int64              `json:"checkpoint"`
	Applied     int64              `json:"applied"`
	Created     int64              `json:"created"`
	Quarantined int64              `json:"quarantined"`
	MaxErrors   int32              `json:"max_errors"`
	CreatedAt   time.Time          `json:"created_at"`
	UpdatedAt   time.Time          `json:"updated_at"`
	Quarantine  []QuarantinedRow   `json:"quarantine"`
}

type QuarantinedRow struct {
	Line   int64  `json:"line"`
	Reason string `json:"reason"`
}

func importFromRow(r db.SourceImport) Import {
	var mapping *ingest.CSVMapping
	if r.Mapping != nil {
		mapping = &ingest.CSVMapping{}
		_ = json.Unmarshal(r.Mapping, mapping)
	}
	return Import{ID: r.ID, Format: r.Format, Mapping: mapping, State: r.State, Checkpoint: r.Checkpoint, Applied: r.Applied, Created: r.Created, Quarantined: r.Quarantined,
		MaxErrors: r.MaxErrors, CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt, Quarantine: []QuarantinedRow{}}
}

// CreateImport opens an import. A CSV import needs a mapping and an NDJSON import must not have one.
func (s Store) CreateImport(ctx context.Context, source string, maxErrors int32, mapping *ingest.CSVMapping) (Import, error) {
	params := db.InsertImportParams{ID: UUID(), Source: source, MaxErrors: maxErrors, Format: "ndjson"}
	if mapping != nil {
		if err := mapping.Validate(); err != nil {
			return Import{}, problem(422, "Invalid import", err.Error())
		}
		params.Format = "csv"
		params.Mapping, _ = json.Marshal(mapping)
	}
	row, err := db.New(s.Pool).InsertImport(ctx, params)
	return importFromRow(row), err
}

// importFormat returns an import's format and mapping for the owning source.
func importFormat(ctx context.Context, q *db.Queries, source, id string) (Import, error) {
	row, err := q.GetImport(ctx, db.GetImportParams{ID: id, Source: source})
	if errors.Is(err, pgx.ErrNoRows) {
		return Import{}, problem(404, "Not found", "Unknown import.")
	}
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
// Rows commit with the checkpoint in chunks, so a client that loses a response reads
// the checkpoint and resends from there; nothing is applied twice and nothing is skipped. Invalid rows
// are quarantined by line and reason and count against max_errors; past it the import fails.
func (s Store) ImportBatch(ctx context.Context, source, id string, offset int64, body io.Reader) (Import, error) {
	if imp, err := importFormat(ctx, db.New(s.Pool), source, id); err != nil {
		return Import{}, err
	} else if imp.Format != "ndjson" {
		return Import{}, problem(415, "Unsupported media type", "This import takes text/csv batches.")
	}
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 4096), importLineBytes)
	chunk := &rowChunk{s: s, ctx: ctx, source: source, id: id}
	line := offset
	for rows := 0; scanner.Scan(); rows++ {
		// After a read error the scanner still yields the truncated rest as a final token; it is not a row.
		if scanner.Err() != nil {
			break
		}
		if rows == importBatchRows {
			return Import{}, problem(413, "Batch too large", fmt.Sprintf("Send at most %d rows per batch.", importBatchRows))
		}
		if err := chunk.add(pendingRow{line: line, raw: append([]byte(nil), scanner.Bytes()...)}); err != nil {
			return Import{}, err
		}
		if chunk.failed {
			break
		}
		line++
	}
	// Rows read before any error commit first, so the error's "rows before it were committed" holds.
	if err := chunk.flush(); err != nil {
		return Import{}, err
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

// pendingRow is one row waiting for its chunk to commit. A preset reason quarantines it unparsed.
type pendingRow struct {
	line   int64
	raw    []byte
	preset string
}

// rowChunk collects rows and commits them importChunkRows at a time. failed reports that the error
// budget failed the import, after which nothing more is read.
type rowChunk struct {
	s          Store
	ctx        context.Context
	source, id string
	rows       []pendingRow
	failed     bool
}

func (c *rowChunk) add(r pendingRow) error {
	c.rows = append(c.rows, r)
	if len(c.rows) < importChunkRows {
		return nil
	}
	return c.flush()
}

func (c *rowChunk) flush() error {
	if len(c.rows) == 0 || c.failed {
		return nil
	}
	failed, err := c.s.importChunk(c.ctx, c.source, c.id, c.rows)
	c.rows, c.failed = c.rows[:0], failed
	return err
}

// importChunk applies consecutive rows and advances the checkpoint in one transaction, so either all of
// them and the checkpoint commit or none do. Each row writes in its own savepoint, so a refused row leaves
// only its quarantine entry. It stops at the row that exhausts the error budget and reports the failure.
func (s Store) importChunk(ctx context.Context, source, id string, rows []pendingRow) (bool, error) {
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
	if rows[0].line != imp.Checkpoint+1 {
		return false, problem(409, "Out of order batch", fmt.Sprintf("Resume this import at line %d.", imp.Checkpoint+1))
	}
	advance := db.AdvanceImportParams{ID: id, State: "open"}
	for _, r := range rows {
		reason, created := r.preset, false
		if reason == "" {
			if reason, created, err = applyImportRow(ctx, tx, source, id, r.line, r.raw); err != nil {
				return false, err
			}
		}
		advance.Checkpoint = r.line
		switch {
		case reason != "":
			if err = q.QuarantineImportRow(ctx, db.QuarantineImportRowParams{ImportID: id, Line: r.line, Reason: reason}); err != nil {
				return false, err
			}
			advance.Quarantined++
		case created:
			advance.Created++
		default:
			advance.Applied++
		}
		if imp.Quarantined+advance.Quarantined > int64(imp.MaxErrors) {
			advance.State = "failed"
			break
		}
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

// ImportCSVBatch applies one CSV batch: a header row, then data rows starting at data row offset. The
// whole body is read first, so a cut-off upload commits nothing rather than half a record. Rows then take
// the same path as NDJSON rows, one transaction each with the checkpoint; a row with the wrong number of
// fields is quarantined, and malformed CSV stops the batch after the rows before it.
func (s Store) ImportCSVBatch(ctx context.Context, source, id string, offset int64, body io.Reader) (Import, error) {
	q := db.New(s.Pool)
	imp, err := importFormat(ctx, q, source, id)
	if err != nil {
		return Import{}, err
	}
	if imp.Format != "csv" {
		return Import{}, problem(415, "Unsupported media type", "This import takes application/x-ndjson batches.")
	}
	raw, err := io.ReadAll(body)
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return Import{}, problem(413, "Batch too large", fmt.Sprintf("The batch body exceeds %d bytes; nothing was committed.", tooLarge.Limit))
	}
	if err != nil {
		return Import{}, err
	}
	reader := csv.NewReader(bytes.NewReader(raw))
	header, err := reader.Read()
	if err != nil {
		return Import{}, problem(400, "Invalid batch", "A CSV batch starts with its header row.")
	}
	index, err := imp.Mapping.Index(header)
	if err != nil {
		return Import{}, problem(400, "Invalid batch", err.Error()+".")
	}
	definitions, err := q.GetCurrentAttributeDefinitions(ctx, imp.Mapping.Paths())
	if err != nil {
		return Import{}, err
	}
	types := map[string]profiles.AttributeType{}
	for _, d := range definitions {
		types[d.Path] = profiles.AttributeType(d.Type)
	}
	chunk := &rowChunk{s: s, ctx: ctx, source: source, id: id}
	line := offset
	for rows := 0; ; rows++ {
		fields, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if rows == importBatchRows {
			return Import{}, problem(413, "Batch too large", fmt.Sprintf("Send at most %d rows per batch.", importBatchRows))
		}
		preset, row := "", []byte(nil)
		switch {
		case errors.Is(err, csv.ErrFieldCount):
			preset = ingest.ReasonInvalidRow
		case err != nil:
			if err := chunk.flush(); err != nil {
				return Import{}, err
			}
			return Import{}, problem(400, "Malformed CSV", fmt.Sprintf("Data row %d is not valid CSV; rows before it were committed.", line))
		default:
			var valid bool
			if row, valid = imp.Mapping.Row(index, fields, types); !valid {
				preset = ingest.ReasonInvalidJSON
			}
		}
		if err = chunk.add(pendingRow{line: line, raw: row, preset: preset}); err != nil {
			return Import{}, err
		}
		if chunk.failed {
			break
		}
		line++
	}
	if err := chunk.flush(); err != nil {
		return Import{}, err
	}
	return s.GetImport(ctx, source, id)
}
