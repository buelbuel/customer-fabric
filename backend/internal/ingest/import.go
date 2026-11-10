package ingest

import (
	"bytes"
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

// ImportRow is one line of a source import: the source's record and the attributes to set or remove.
// A row for a record the source does not have yet creates it; remove is then refused.
type ImportRow struct {
	Record Record                     `json:"record"`
	Set    map[string]json.RawMessage `json:"set,omitempty"`
	Remove []string                   `json:"remove,omitempty"`
}

// Quarantine reasons a row can be refused for before any write. They are codes, never row content.
const (
	ReasonInvalidJSON = "invalid_json"
	ReasonInvalidRow  = "invalid_row"
)

// ParseImportRow decodes one line strictly: known fields only, no duplicate keys, no nulls. It returns
// a quarantine reason instead of an error, because a bad row must not stop the import.
func ParseImportRow(line []byte, duplicateKeys func([]byte) bool) (ImportRow, string) {
	var row ImportRow
	var fields map[string]json.RawMessage
	if !utf8.Valid(line) || !json.Valid(line) || duplicateKeys(line) || json.Unmarshal(line, &fields) != nil || fields == nil {
		return row, ReasonInvalidJSON
	}
	for name, value := range fields {
		if (name != "record" && name != "set" && name != "remove") || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return row, ReasonInvalidRow
		}
	}
	var record map[string]json.RawMessage
	if json.Unmarshal(fields["record"], &record) != nil {
		return row, ReasonInvalidRow
	}
	for name := range record {
		if name != "namespace" && name != "value" {
			return row, ReasonInvalidRow
		}
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if dec.Decode(&row) != nil {
		return row, ReasonInvalidRow
	}
	if !profiles.Name.MatchString(row.Record.Namespace) || row.Record.Value == "" || len(row.Record.Value) > 256 || strings.ContainsRune(row.Record.Value, 0) {
		return row, ReasonInvalidRow
	}
	if len(row.Set) == 0 && len(row.Remove) == 0 {
		return row, ReasonInvalidRow
	}
	if (profiles.Mutation{Set: row.Set, Remove: row.Remove}).Validate(false) != nil {
		return row, ReasonInvalidRow
	}
	return row, ""
}

// Mutation is the write the row asks for. Creating a record adds its identifier and cannot remove.
func (r ImportRow) Mutation(create bool) profiles.Mutation {
	if create {
		return profiles.Mutation{Set: r.Set, Identifiers: []profiles.IdentifierInput{{Namespace: r.Record.Namespace, Value: r.Record.Value}}}
	}
	return profiles.Mutation{Set: r.Set, Remove: r.Remove}
}
