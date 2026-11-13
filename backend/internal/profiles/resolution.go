package profiles

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
)

// Contribution is one source record's value for one attribute of a unified customer (ADR 0018). A nil
// Value is the record's withdrawal, its tombstone: it never blocks another record's value.
type Contribution struct {
	Record        string
	Source        string
	Value         json.RawMessage
	ContributedAt time.Time
	UpdatedAt     time.Time
}

// ResolutionPolicy is an attribute's configured rule.
type ResolutionPolicy struct {
	// Strategy is source_priority or most_recent.
	Strategy       string
	SourcePriority []string
}

// Strategies are the configurable resolution policies.
var Strategies = []string{"source_priority", "most_recent"}

// Resolve chooses the contribution a unified customer shows for one attribute; false when no record has
// a live value. It depends only on its inputs, never on their order:
//   - source_priority prefers the first listed source with a live value; unlisted sources rank after
//     every listed one;
//   - most_recent prefers the most recently updated value;
//   - without a policy, and between equally ranked values, the earliest value still held wins, then the
//     lower record ID. A later, different value from another record therefore never displaces it.
func Resolve(policy *ResolutionPolicy, contributions []Contribution) (Contribution, bool) {
	live := slices.DeleteFunc(slices.Clone(contributions), func(c Contribution) bool { return c.Value == nil })
	if len(live) == 0 {
		return Contribution{}, false
	}
	rank := func(Contribution) int64 { return 0 }
	if policy != nil {
		switch policy.Strategy {
		case "source_priority":
			rank = func(c Contribution) int64 {
				if i := slices.Index(policy.SourcePriority, c.Source); i >= 0 {
					return int64(i)
				}
				return int64(len(policy.SourcePriority))
			}
		case "most_recent":
			rank = func(c Contribution) int64 { return -c.UpdatedAt.UnixNano() }
		}
	}
	return slices.MinFunc(live, func(a, b Contribution) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			if ra < rb {
				return -1
			}
			return 1
		}
		if c := a.ContributedAt.Compare(b.ContributedAt); c != 0 {
			return c
		}
		return strings.Compare(a.Record, b.Record)
	}), true
}
