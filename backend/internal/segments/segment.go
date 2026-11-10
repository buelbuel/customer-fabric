// Package segments defines typed audience rules over governed attributes and evaluates them against a
// customer's segmentation view. Rules are data, never SQL: a client cannot supply a query.
package segments

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

// Logic names the evaluation semantics a preview was computed with.
const Logic = "segment-rules-v1"

const (
	maxConditions = 20
	maxListValues = 100
	maxDays       = 36500
)

type Match string

const (
	MatchAll Match = "all"
	MatchAny Match = "any"
)

type Operator string

const (
	OpEq         Operator = "eq"
	OpNeq        Operator = "neq"
	OpIn         Operator = "in"
	OpNotIn      Operator = "not_in"
	OpGt         Operator = "gt"
	OpGte        Operator = "gte"
	OpLt         Operator = "lt"
	OpLte        Operator = "lte"
	OpPresent    Operator = "present"
	OpMissing    Operator = "missing"
	OpWithinDays Operator = "within_last_days"
)

// Condition tests one governed attribute. Sources, when given, limits which owning sources' values count.
type Condition struct {
	Attribute string            `json:"attribute"`
	Operator  Operator          `json:"operator"`
	Value     json.RawMessage   `json:"value,omitempty"`
	Values    []json.RawMessage `json:"values,omitempty"`
	Days      *int32            `json:"days,omitempty"`
	Sources   []string          `json:"sources,omitempty"`
}

// Rule is what a segment revision stores. Members must also permit PurposeID; that is not optional, so
// no segment can select customers without an explicit consent criterion.
type Rule struct {
	PurposeID  string      `json:"purpose_id"`
	Match      Match       `json:"match"`
	Conditions []Condition `json:"conditions"`
}

// Segment is one immutable revision.
type Segment struct {
	Name      string    `json:"name"`
	Revision  int32     `json:"revision"`
	Rule      Rule      `json:"rule"`
	CreatedAt time.Time `json:"created_at"`
}

func operand(t profiles.AttributeType, op Operator) bool {
	switch op {
	case OpPresent, OpMissing, OpEq, OpNeq:
		return true
	case OpIn, OpNotIn:
		return t == profiles.TypeString || t == profiles.TypeEnum || t == profiles.TypeInteger
	case OpGt, OpGte, OpLt, OpLte:
		return t == profiles.TypeInteger || t == profiles.TypeDecimal || t == profiles.TypeDate || t == profiles.TypeTimestamp
	case OpWithinDays:
		return t == profiles.TypeDate || t == profiles.TypeTimestamp
	}
	return false
}

// Validate checks a rule against the current definitions of its attributes. Literals are checked with
// the attribute's own definition, so a rule can only compare values the attribute could hold. Errors
// name rule positions, never values.
func (r Rule) Validate(definitions map[string]profiles.AttributeDefinition) error {
	if r.Match != MatchAll && r.Match != MatchAny {
		return fmt.Errorf("match must be all or any")
	}
	if len(r.Conditions) == 0 || len(r.Conditions) > maxConditions {
		return fmt.Errorf("a rule has between 1 and %d conditions", maxConditions)
	}
	for i, c := range r.Conditions {
		if err := c.validate(definitions); err != nil {
			return fmt.Errorf("condition %d: %w", i, err)
		}
	}
	return nil
}

func (c Condition) validate(definitions map[string]profiles.AttributeDefinition) error {
	d, defined := definitions[c.Attribute]
	if !defined {
		return fmt.Errorf("attribute must be defined")
	}
	if !operand(d.Type, c.Operator) {
		return fmt.Errorf("operator %q does not apply to a %s attribute", c.Operator, d.Type)
	}
	single := c.Operator != OpIn && c.Operator != OpNotIn && c.Operator != OpPresent && c.Operator != OpMissing && c.Operator != OpWithinDays
	switch {
	case single != (c.Value != nil):
		return fmt.Errorf("value is required by %s and allowed only there", "eq, neq, gt, gte, lt and lte")
	case (c.Operator == OpIn || c.Operator == OpNotIn) != (c.Values != nil):
		return fmt.Errorf("values is required by in and not_in and allowed only there")
	case (c.Operator == OpWithinDays) != (c.Days != nil):
		return fmt.Errorf("days is required by within_last_days and allowed only there")
	case c.Values != nil && (len(c.Values) == 0 || len(c.Values) > maxListValues):
		return fmt.Errorf("values holds between 1 and %d literals", maxListValues)
	case c.Days != nil && (*c.Days < 1 || *c.Days > maxDays):
		return fmt.Errorf("days must be between 1 and %d", maxDays)
	case len(c.Sources) > 20:
		return fmt.Errorf("sources names at most 20 sources")
	}
	seen := map[string]bool{}
	for _, s := range c.Sources {
		if !profiles.Name.MatchString(s) || seen[s] {
			return fmt.Errorf("sources must be distinct source names")
		}
		seen[s] = true
	}
	literals := c.Values
	if c.Value != nil {
		literals = []json.RawMessage{c.Value}
	}
	for _, l := range literals {
		if d.Check(l) != nil {
			return fmt.Errorf("every literal must be a valid %s value of the attribute", d.Type)
		}
	}
	return nil
}

// Evidence points at a value that decided a condition, by provenance only. A preview reader may not be
// granted the value itself, so it is never included.
type Evidence struct {
	Source            string `json:"source"`
	AssociationID     string `json:"association_id"`
	RecordProfileID   string `json:"record_profile_id"`
	MutationID        string `json:"mutation_id"`
	AttributeRevision *int32 `json:"attribute_revision,omitempty"`
}

// Outcome is one condition's result for one customer.
type Outcome struct {
	Index     int        `json:"index"`
	Attribute string     `json:"attribute"`
	Operator  Operator   `json:"operator"`
	Matched   bool       `json:"matched"`
	Evidence  []Evidence `json:"evidence"`
}

// Evaluate applies a rule to one customer's segmentation view at an evaluation time.
//
// A view holds every visible value of an attribute, one per contributing source record, and values the
// segmentation reader is not granted are absent. For the visible values V of a condition:
//   - missing matches when V is empty and present when it is not;
//   - neq and not_in match when V is non-empty and every value passes, so a missing value never
//     satisfies a negative comparison and a conflicting source cannot be ignored;
//   - every other operator matches when some value passes.
//
// Numbers compare exactly (1 equals 1.0), dates as calendar dates, timestamps as instants, and strings
// and enums byte for byte with no locale folding. within_last_days uses the UTC calendar date of at.
func (r Rule) Evaluate(view profiles.CustomerView, at time.Time) (bool, []Outcome) {
	attributes := map[string]profiles.ViewAttribute{}
	for _, a := range view.Attributes {
		attributes[a.Path] = a
	}
	outcomes := make([]Outcome, 0, len(r.Conditions))
	all, any := true, false
	for i, c := range r.Conditions {
		o := c.evaluate(attributes[c.Attribute], at)
		o.Index = i
		if !o.Matched {
			o.Evidence = []Evidence{}
		}
		outcomes = append(outcomes, o)
		all, any = all && o.Matched, any || o.Matched
	}
	if r.Match == MatchAny {
		return any, outcomes
	}
	return all, outcomes
}

func (c Condition) evaluate(attribute profiles.ViewAttribute, at time.Time) Outcome {
	o := Outcome{Attribute: c.Attribute, Operator: c.Operator, Evidence: []Evidence{}}
	values := []profiles.ViewValue{}
	for _, v := range attribute.Values {
		if len(c.Sources) == 0 || slices.Contains(c.Sources, v.Source) {
			values = append(values, v)
		}
	}
	cite := func(v profiles.ViewValue) {
		o.Evidence = append(o.Evidence, Evidence{Source: v.Source, AssociationID: v.AssociationID, RecordProfileID: v.RecordProfileID,
			MutationID: v.MutationID, AttributeRevision: v.AttributeRevision})
	}
	switch c.Operator {
	case OpMissing:
		o.Matched = len(values) == 0
		return o
	case OpPresent, OpNeq, OpNotIn:
		o.Matched = len(values) > 0
		for _, v := range values {
			cite(v)
			if c.Operator != OpPresent && c.passes(attribute.Type, v.Value, at) {
				o.Matched = false
			}
		}
		return o
	}
	for _, v := range values {
		if c.passes(attribute.Type, v.Value, at) {
			o.Matched = true
			cite(v)
		}
	}
	return o
}

// passes reports the positive test: equality for eq and neq, membership for in and not_in.
func (c Condition) passes(t profiles.AttributeType, value json.RawMessage, at time.Time) bool {
	switch c.Operator {
	case OpEq, OpNeq:
		cmp, ok := compare(t, value, c.Value)
		return ok && cmp == 0
	case OpIn, OpNotIn:
		for _, l := range c.Values {
			if cmp, ok := compare(t, value, l); ok && cmp == 0 {
				return true
			}
		}
		return false
	case OpGt, OpGte, OpLt, OpLte:
		cmp, ok := compare(t, value, c.Value)
		return ok && map[Operator]bool{OpGt: cmp > 0, OpGte: cmp >= 0, OpLt: cmp < 0, OpLte: cmp <= 0}[c.Operator]
	case OpWithinDays:
		return within(t, value, at, int(*c.Days))
	}
	return false
}

// compare orders two values of type t. ok is false for a value that does not parse, which can only be
// one stored before its attribute was defined and that adoption therefore checked; it never matches.
func compare(t profiles.AttributeType, a, b json.RawMessage) (int, bool) {
	switch t {
	case profiles.TypeInteger, profiles.TypeDecimal:
		x, okA := new(big.Rat).SetString(string(a))
		y, okB := new(big.Rat).SetString(string(b))
		if !okA || !okB {
			return 0, false
		}
		return x.Cmp(y), true
	case profiles.TypeDate, profiles.TypeTimestamp:
		x, okA := instant(t, a)
		y, okB := instant(t, b)
		if !okA || !okB {
			return 0, false
		}
		return x.Compare(y), true
	case profiles.TypeBoolean:
		if string(a) == string(b) {
			return 0, true
		}
		return 1, true
	}
	var x, y string
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return 0, false
	}
	switch {
	case x == y:
		return 0, true
	case x < y:
		return -1, true
	}
	return 1, true
}

func instant(t profiles.AttributeType, raw json.RawMessage) (time.Time, bool) {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return time.Time{}, false
	}
	layout := time.RFC3339Nano
	if t == profiles.TypeDate {
		layout = time.DateOnly
	}
	v, err := time.Parse(layout, s)
	return v, err == nil
}

func within(t profiles.AttributeType, raw json.RawMessage, at time.Time, days int) bool {
	v, ok := instant(t, raw)
	if !ok {
		return false
	}
	if t == profiles.TypeDate {
		today := at.UTC().Truncate(24 * time.Hour)
		return !v.Before(today.AddDate(0, 0, -days)) && !v.After(today)
	}
	return !v.Before(at.AddDate(0, 0, -days)) && !v.After(at)
}
