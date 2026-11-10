package ingest

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

// CSVMapping says how a CSV import's columns become a record: which column holds the source's record
// value in which namespace, and which columns set which attributes. Unmapped columns are ignored.
type CSVMapping struct {
	Namespace    string            `json:"namespace"`
	RecordColumn string            `json:"record_column"`
	Columns      map[string]string `json:"columns"`
}

func (m CSVMapping) Validate() error {
	if !profiles.Name.MatchString(m.Namespace) {
		return fmt.Errorf("namespace must be a lowercase identifier")
	}
	if m.RecordColumn == "" || len(m.RecordColumn) > 128 {
		return fmt.Errorf("record_column must name a column of at most 128 bytes")
	}
	if len(m.Columns) == 0 || len(m.Columns) > 100 {
		return fmt.Errorf("columns maps between 1 and 100 columns to attributes")
	}
	paths := map[string]bool{}
	for column, path := range m.Columns {
		if column == "" || len(column) > 128 || column == m.RecordColumn || !profiles.Name.MatchString(path) || paths[path] {
			return fmt.Errorf("each column maps to a distinct attribute name, and the record column is not an attribute")
		}
		paths[path] = true
	}
	return nil
}

// Paths lists the attributes the mapping sets.
func (m CSVMapping) Paths() []string {
	paths := make([]string, 0, len(m.Columns))
	for _, path := range m.Columns {
		paths = append(paths, path)
	}
	return paths
}

// Index locates the mapped columns in a header row. Every mapped column must be present exactly once;
// unmapped columns are ignored, even when their names repeat.
func (m CSVMapping) Index(header []string) (map[string]int, error) {
	index := map[string]int{}
	for i, name := range header {
		name = strings.TrimPrefix(name, "\uFEFF")
		_, mapped := m.Columns[name]
		if name != m.RecordColumn && !mapped {
			continue
		}
		if _, dup := index[name]; dup {
			return nil, fmt.Errorf("the header repeats mapped column %d", i+1)
		}
		index[name] = i
	}
	if _, ok := index[m.RecordColumn]; !ok {
		return nil, fmt.Errorf("the header lacks the record column")
	}
	for column := range m.Columns {
		if _, ok := index[column]; !ok {
			return nil, fmt.Errorf("the header lacks a mapped column")
		}
	}
	return index, nil
}

// Row turns one CSV record into an import row line, or reports false when a used cell is not valid UTF-8:
// encoding it would silently replace bytes, and two distinct record values could then become one. An empty cell sets nothing. A cell is encoded by its
// attribute's defined type: integer and decimal cells that are JSON numbers become numbers and boolean cells
// true or false become booleans; anything else stays a string, so a defined type still refuses it.
func (m CSVMapping) Row(index map[string]int, fields []string, types map[string]profiles.AttributeType) ([]byte, bool) {
	value := fields[index[m.RecordColumn]]
	if !utf8.ValidString(value) {
		return nil, false
	}
	row := ImportRow{Record: Record{Namespace: m.Namespace, Value: value}, Set: map[string]json.RawMessage{}}
	for column, path := range m.Columns {
		cell := fields[index[column]]
		if !utf8.ValidString(cell) {
			return nil, false
		}
		if cell == "" {
			continue
		}
		row.Set[path] = encodeCell(cell, types[path])
	}
	line, _ := json.Marshal(row)
	return line, true
}

func encodeCell(cell string, t profiles.AttributeType) json.RawMessage {
	var number json.Number
	switch {
	case (t == profiles.TypeInteger || t == profiles.TypeDecimal) && cell[0] != '"' && json.Unmarshal([]byte(cell), &number) == nil:
		return json.RawMessage(cell)
	case t == profiles.TypeBoolean && (cell == "true" || cell == "false"):
		return json.RawMessage(cell)
	}
	raw, _ := json.Marshal(cell)
	return raw
}
