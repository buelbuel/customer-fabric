// Package ingest defines the inbound source event contract and its ordering rule. It has no
// HTTP or database dependency.
package ingest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

// RecordUpdated is the only supported event type.
const RecordUpdated = "record.updated.v1"

var eventID = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// Record names the source's own record. The source itself comes from the credential.
type Record struct {
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
}

// Event is one change reported by a source. OccurredAt is informational and never orders anything.
type Event struct {
	EventID     string                     `json:"event_id"`
	Type        string                     `json:"type"`
	Record      Record                     `json:"record"`
	SourceEpoch int64                      `json:"source_epoch"`
	Sequence    int64                      `json:"sequence"`
	OccurredAt  time.Time                  `json:"occurred_at"`
	Set         map[string]json.RawMessage `json:"set,omitempty"`
	Remove      []string                   `json:"remove,omitempty"`
}

// Mutation is the attribute change the event asks for. An event cannot add identifiers.
func (e Event) Mutation() profiles.Mutation {
	return profiles.Mutation{Set: e.Set, Remove: e.Remove}
}

func (e Event) Validate() error {
	if !eventID.MatchString(e.EventID) {
		return fmt.Errorf("event_id must be 1 to 128 letters, digits, dots, colons, underscores or hyphens")
	}
	if e.Type != RecordUpdated {
		return fmt.Errorf("unsupported event type")
	}
	if !profiles.Name.MatchString(e.Record.Namespace) || e.Record.Value == "" || len(e.Record.Value) > 256 || strings.ContainsRune(e.Record.Value, 0) {
		return fmt.Errorf("record requires a namespace and a value of at most 256 bytes")
	}
	if e.SourceEpoch < 1 || e.Sequence < 1 {
		return fmt.Errorf("source_epoch and sequence must be positive")
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("occurred_at is required")
	}
	return e.Mutation().Validate(false)
}

// Digest identifies the event's content, so a reused event_id with different content is detected.
func (e Event) Digest() string {
	e.OccurredAt = e.OccurredAt.UTC()
	canonical, _ := json.Marshal(e)
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Position is a point in one record's event order.
type Position struct {
	SourceEpoch, Sequence int64
}

// After reports whether p is strictly later than o. An equal position is not later, so a second
// event claiming an already used sequence is stale rather than decided by arrival order.
func (p Position) After(o Position) bool {
	if p.SourceEpoch != o.SourceEpoch {
		return p.SourceEpoch > o.SourceEpoch
	}
	return p.Sequence > o.Sequence
}
