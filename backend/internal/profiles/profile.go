// Package profiles defines the initial scalar attribute ownership policy.
package profiles

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

var Name = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
var ID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Identifier struct {
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
	Path       string    `json:"path"`
	Source     string    `json:"source"`
	MutationID string    `json:"mutation_id"`
	At         time.Time `json:"at"`
	Outcome    string    `json:"outcome"`
	Rule       string    `json:"rule"`
}
type Profile struct {
	ID          string                     `json:"id"`
	Version     int64                      `json:"version"`
	Attributes  map[string]json.RawMessage `json:"attributes"`
	Identifiers []Identifier               `json:"identifiers"`
	Provenance  []Decision                 `json:"provenance"`
	CreatedAt   time.Time                  `json:"created_at"`
	UpdatedAt   time.Time                  `json:"updated_at"`
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
			if str, ok := x.(string); ok && strings.ContainsRune(str, 0) {
				return fmt.Errorf("NUL characters are unsupported")
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
func (m Mutation) Paths() []string {
	paths := make([]string, 0, len(m.Set)+len(m.Remove))
	for k := range m.Set {
		paths = append(paths, k)
	}
	paths = append(paths, m.Remove...)
	sort.Strings(paths)
	return paths
}

// CheckOwnership retains ownership even after removal, preventing resurrection by another source.
func CheckOwnership(m Mutation, owners map[string]string, source string) []string {
	conflicts := []string{}
	for _, k := range m.Paths() {
		if owner := owners[k]; owner != "" && owner != source {
			conflicts = append(conflicts, k)
		}
	}
	return conflicts
}
