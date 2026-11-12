// Package profiles defines source profile mutations and unified attribute resolution.
package profiles

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var Name = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var ID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Identifier struct {
	// ProfileID is the source record holding the identifier, in a unified customer.
	ProfileID string `json:"profile_id,omitempty"`
	Source    string `json:"source,omitempty"`
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
}
type IdentifierInput struct {
	Namespace string `json:"namespace"`
	Value     string `json:"value"`
}

type Mutation struct {
	Set         map[string]json.RawMessage `json:"set"`
	Remove      []string                   `json:"remove"`
	Identifiers []IdentifierInput          `json:"identifiers"`
}
type Decision struct {
	// ProfileID is the source record decided on, in a unified customer.
	ProfileID  string    `json:"profile_id,omitempty"`
	Path       string    `json:"path"`
	Source     string    `json:"source"`
	MutationID string    `json:"mutation_id"`
	At         time.Time `json:"at"`
	Outcome    string    `json:"outcome"`
	Rule       string    `json:"rule"`
	// AttributeRevision is the definition revision an accepted value was validated under.
	AttributeRevision *int32 `json:"attribute_revision,omitempty"`
}

// Customer is a unified customer profile (ADR 0018): its source records and the attributes resolved
// from them. ResolvedFrom names the record each resolved attribute comes from; every other record's
// value stays visible in Records.
type Customer struct {
	ID           string                     `json:"id"`
	Aliases      []string                   `json:"aliases"`
	Attributes   map[string]json.RawMessage `json:"attributes"`
	ResolvedFrom map[string]string          `json:"resolved_from"`
	Records      []SourceRecord             `json:"records"`
	Identifiers  []Identifier               `json:"identifiers"`
	Provenance   []Decision                 `json:"provenance"`
	CreatedAt    time.Time                  `json:"created_at"`
	UpdatedAt    time.Time                  `json:"updated_at"`
}

// SourceRecord is one source profile within a unified customer, with its own values.
type SourceRecord struct {
	ID         string                     `json:"id"`
	Source     string                     `json:"source,omitempty"`
	Version    int64                      `json:"version"`
	Attributes map[string]json.RawMessage `json:"attributes"`
	CreatedAt  time.Time                  `json:"created_at"`
	UpdatedAt  time.Time                  `json:"updated_at"`
}

type Receipt struct {
	ID         string `json:"id"`
	Version    int64  `json:"version"`
	MutationID string `json:"mutation_id"`
}

func (m Mutation) Validate(create bool) error {
	if len(m.Set)+len(m.Remove)+len(m.Identifiers) == 0 {
		return fmt.Errorf("at least one mutation is required")
	}
	if len(m.Set)+len(m.Remove) > 100 || len(m.Identifiers) > 20 {
		return fmt.Errorf("mutation exceeds field or identifier limit")
	}
	if create && len(m.Remove) > 0 {
		return fmt.Errorf("remove is unsupported during creation")
	}
	seen := map[string]bool{}
	for k, v := range m.Set {
		if !Name.MatchString(k) {
			return fmt.Errorf("invalid attribute name")
		}
		var x any
		if json.Unmarshal(v, &x) != nil {
			return fmt.Errorf("invalid attribute value")
		}
		switch x.(type) {
		case string, bool, float64:
			if str, ok := x.(string); ok && (strings.ContainsRune(str, 0) || loneSurrogate(v)) {
				return fmt.Errorf("NUL characters and unpaired surrogates are unsupported")
			}
		default:
			return fmt.Errorf("only non-null scalar attributes are supported")
		}
		if len(v) > 4096 {
			return fmt.Errorf("attribute value exceeds 4096 bytes")
		}
		seen[k] = true
	}
	for _, k := range m.Remove {
		if !Name.MatchString(k) || seen[k] {
			return fmt.Errorf("invalid, repeated, or overlapping removal")
		}
		seen[k] = true
	}
	seen = map[string]bool{}
	for _, i := range m.Identifiers {
		if !Name.MatchString(i.Namespace) || len(i.Value) == 0 || len(i.Value) > 256 || strings.ContainsRune(i.Value, 0) {
			return fmt.Errorf("identifier requires namespace and value; source is credential-derived")
		}
		key := i.Namespace + "\x00" + i.Value
		if seen[key] {
			return fmt.Errorf("duplicate identifier")
		}
		seen[key] = true
	}
	return nil
}

// loneSurrogate finds an unpaired surrogate escape in a JSON string. Go decodes one to U+FFFD, but
// the raw value is what reaches jsonb, which rejects it.
func loneSurrogate(raw []byte) bool {
	unit := func(i int) (uint64, bool) {
		if i+6 > len(raw) || raw[i] != '\\' || raw[i+1] != 'u' {
			return 0, false
		}
		n, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
		return n, err == nil
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		n, ok := unit(i)
		if !ok {
			i++
			continue
		}
		i += 5
		switch {
		case n >= 0xDC00 && n <= 0xDFFF:
			return true
		case n >= 0xD800 && n <= 0xDBFF:
			low, paired := unit(i + 1)
			if !paired || low < 0xDC00 || low > 0xDFFF {
				return true
			}
			i += 6
		}
	}
	return false
}

func (m Mutation) Paths() []string {
	paths := make([]string, 0, len(m.Set)+len(m.Remove))
	for k := range m.Set {
		paths = append(paths, k)
	}
	paths = append(paths, m.Remove...)
	sort.Strings(paths)
	return paths
}
