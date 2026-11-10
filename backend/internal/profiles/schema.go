package profiles

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// AttributeType is the type a definition gives an attribute. Every type stores a JSON scalar, so a
// typed attribute keeps the existing flat attribute shape.
type AttributeType string

const (
	TypeString    AttributeType = "string"
	TypeInteger   AttributeType = "integer"
	TypeDecimal   AttributeType = "decimal"
	TypeBoolean   AttributeType = "boolean"
	TypeDate      AttributeType = "date"
	TypeTimestamp AttributeType = "timestamp"
	TypeEnum      AttributeType = "enum"
)

// maxSafeInteger keeps integers exact in JSON clients and in PostgreSQL numeric comparisons.
const maxSafeInteger = 1<<53 - 1

var integerLiteral = regexp.MustCompile(`^-?(0|[1-9][0-9]*)$`)

// AttributeDefinition is one immutable revision of an attribute's type. Revisions of a path keep its
// type and only widen what it accepts, so every value accepted under an earlier revision stays valid.
type AttributeDefinition struct {
	Path      string        `json:"path"`
	Revision  int32         `json:"revision"`
	Type      AttributeType `json:"type"`
	MaxLength *int32        `json:"max_length,omitempty"`
	Values    []string      `json:"values,omitempty"`
	CreatedAt time.Time     `json:"created_at"`
}

func (d AttributeDefinition) Validate() error {
	if !Name.MatchString(d.Path) {
		return fmt.Errorf("attribute names are lowercase identifiers of at most 64 characters")
	}
	switch d.Type {
	case TypeString, TypeInteger, TypeDecimal, TypeBoolean, TypeDate, TypeTimestamp, TypeEnum:
	default:
		return fmt.Errorf("type must be string, integer, decimal, boolean, date, timestamp or enum")
	}
	if d.MaxLength != nil && (d.Type != TypeString || *d.MaxLength < 1 || *d.MaxLength > 4096) {
		return fmt.Errorf("max_length applies only to strings and must be between 1 and 4096")
	}
	if (d.Type == TypeEnum) != (d.Values != nil) || (d.Values != nil && len(d.Values) == 0) {
		return fmt.Errorf("values are required for an enum and allowed only for an enum")
	}
	if len(d.Values) > 100 {
		return fmt.Errorf("an enum allows at most 100 values")
	}
	seen := map[string]bool{}
	for _, v := range d.Values {
		if v == "" || len(v) > 64 || strings.ContainsRune(v, 0) || !utf8.ValidString(v) || seen[v] {
			return fmt.Errorf("enum values must be distinct, non-empty and at most 64 bytes")
		}
		seen[v] = true
	}
	return nil
}

// Supersedes reports whether d may follow prev as the next revision of the same attribute. A
// revision that would refuse a value prev accepted is incompatible: use a new attribute name instead.
func (d AttributeDefinition) Supersedes(prev AttributeDefinition) error {
	if d.Type != prev.Type {
		return fmt.Errorf("an attribute's type cannot change; define a new attribute instead")
	}
	switch d.Type {
	case TypeString:
		if prev.MaxLength == nil || (d.MaxLength != nil && *d.MaxLength <= *prev.MaxLength) {
			return fmt.Errorf("a string revision may only raise or remove max_length")
		}
	case TypeEnum:
		for _, v := range prev.Values {
			if !slices.Contains(d.Values, v) {
				return fmt.Errorf("an enum revision must keep every existing value")
			}
		}
		if len(d.Values) == len(prev.Values) {
			return fmt.Errorf("an enum revision must add at least one value")
		}
	default:
		return fmt.Errorf("a %s attribute has nothing to revise", d.Type)
	}
	return nil
}

// Check validates one stored value. The error never contains the value, which is customer data.
func (d AttributeDefinition) Check(raw json.RawMessage) error {
	refuse := func(rule string) error {
		return fmt.Errorf("attribute %s must be %s under revision %d", d.Path, rule, d.Revision)
	}
	raw = bytes.TrimSpace(raw)
	var str string
	isString := json.Unmarshal(raw, &str) == nil && len(raw) > 0 && raw[0] == '"'
	switch d.Type {
	case TypeString:
		if !isString {
			return refuse("a string")
		}
		if d.MaxLength != nil && utf8.RuneCountInString(str) > int(*d.MaxLength) {
			return refuse(fmt.Sprintf("at most %d characters", *d.MaxLength))
		}
	case TypeInteger:
		n, err := strconv.ParseInt(string(raw), 10, 64)
		if !integerLiteral.Match(raw) || err != nil || n > maxSafeInteger || n < -maxSafeInteger {
			return refuse("an integer literal between -(2^53-1) and 2^53-1")
		}
	case TypeDecimal:
		var n json.Number
		if len(raw) == 0 || raw[0] == '"' || json.Unmarshal(raw, &n) != nil {
			return refuse("a JSON number")
		}
		if _, err := n.Float64(); err != nil {
			return refuse("a finite JSON number")
		}
	case TypeBoolean:
		if string(raw) != "true" && string(raw) != "false" {
			return refuse("a boolean")
		}
	case TypeDate:
		t, err := time.Parse(time.DateOnly, str)
		if !isString || err != nil || t.Year() < 1 {
			return refuse("a YYYY-MM-DD date from year 0001")
		}
	case TypeTimestamp:
		// PostgreSQL has no year 0, so the bound keeps every accepted value castable.
		t, err := time.Parse(time.RFC3339Nano, str)
		if !isString || err != nil || t.UTC().Year() < 1 || t.UTC().Year() > 9999 {
			return refuse("an RFC 3339 timestamp with an offset, in UTC years 1 to 9999")
		}
	case TypeEnum:
		if !isString || !slices.Contains(d.Values, str) {
			return refuse("one of the defined values")
		}
	default:
		return refuse("of a supported type")
	}
	return nil
}

// Reader is a use of governed attribute values. Customers are deliberately not a reader: customer
// access to source attributes needs its own authorization decision (ADR 0007).
type Reader string

const (
	ReaderOperator     Reader = "operator"
	ReaderSegmentation Reader = "segmentation"
	ReaderActivation   Reader = "activation"
	ReaderMatching     Reader = "matching"
)

// AccessPolicy is one immutable revision of who may read a defined attribute and whose values. Unlike
// a definition it may narrow, so access can be withdrawn. Without a policy nothing is readable.
type AccessPolicy struct {
	Path      string    `json:"path"`
	Revision  int32     `json:"revision"`
	Readers   []Reader  `json:"readers"`
	Sources   []string  `json:"sources"`
	CreatedAt time.Time `json:"created_at"`
}

func (p AccessPolicy) Validate() error {
	if p.Readers == nil || p.Sources == nil {
		return fmt.Errorf("readers and sources are required; use empty lists to withdraw access")
	}
	if len(p.Sources) > 20 {
		return fmt.Errorf("a policy names at most 20 sources")
	}
	readers := map[Reader]bool{}
	for _, r := range p.Readers {
		if (r != ReaderOperator && r != ReaderSegmentation && r != ReaderActivation && r != ReaderMatching) || readers[r] {
			return fmt.Errorf("readers must be distinct values of operator, segmentation, activation and matching")
		}
		readers[r] = true
	}
	sources := map[string]bool{}
	for _, s := range p.Sources {
		if !Name.MatchString(s) || sources[s] {
			return fmt.Errorf("sources must be distinct source names")
		}
		sources[s] = true
	}
	return nil
}

// Allows reports whether reader may see the value an owning source wrote.
func (p AccessPolicy) Allows(reader Reader, source string) bool {
	return slices.Contains(p.Readers, reader) && slices.Contains(p.Sources, source)
}
