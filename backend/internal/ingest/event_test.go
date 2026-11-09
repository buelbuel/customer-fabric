package ingest

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func valid() Event {
	return Event{
		EventID: "evt-1", Type: RecordUpdated, Record: Record{"crm", "record-1"}, SourceEpoch: 1, Sequence: 1,
		OccurredAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), Set: map[string]json.RawMessage{"name": json.RawMessage(`"Synthetic"`)},
	}
}

func TestValidate(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Event){
		"event id":     func(e *Event) { e.EventID = "has space" },
		"long id":      func(e *Event) { e.EventID = strings.Repeat("e", 129) },
		"type":         func(e *Event) { e.Type = "record.deleted.v1" },
		"namespace":    func(e *Event) { e.Record.Namespace = "Not Valid" },
		"empty value":  func(e *Event) { e.Record.Value = "" },
		"long value":   func(e *Event) { e.Record.Value = strings.Repeat("v", 257) },
		"nul value":    func(e *Event) { e.Record.Value = "a\x00b" },
		"source epoch": func(e *Event) { e.SourceEpoch = 0 },
		"sequence":     func(e *Event) { e.Sequence = 0 },
		"occurred at":  func(e *Event) { e.OccurredAt = time.Time{} },
		"overflows utc": func(e *Event) {
			e.OccurredAt = time.Date(9999, 12, 31, 23, 59, 59, 0, time.FixedZone("", -3600))
		},
		"no change":  func(e *Event) { e.Set = nil },
		"null value": func(e *Event) { e.Set = map[string]json.RawMessage{"name": json.RawMessage(`null`)} },
		"overlap":    func(e *Event) { e.Remove = []string{"name"} },
	} {
		t.Run(name, func(t *testing.T) {
			e := valid()
			mutate(&e)
			if e.Validate() == nil {
				t.Fatal("accepted")
			}
		})
	}
}

func TestDigestCoversContent(t *testing.T) {
	base := valid().Digest()
	if valid().Digest() != base {
		t.Fatal("digest is not deterministic")
	}
	offset := valid()
	offset.OccurredAt = offset.OccurredAt.In(time.FixedZone("", 3600))
	if offset.Digest() != base {
		t.Fatal("the same instant in another offset changed the digest")
	}
	reordered, original := valid(), valid()
	reordered.Remove, original.Remove = []string{"b", "a"}, []string{"a", "b"}
	if reordered.Digest() != original.Digest() {
		t.Fatal("the order of remove changed the digest")
	}
	for name, mutate := range map[string]func(*Event){
		"value":    func(e *Event) { e.Set["name"] = json.RawMessage(`"Other"`) },
		"record":   func(e *Event) { e.Record.Value = "record-2" },
		"sequence": func(e *Event) { e.Sequence = 2 },
		"epoch":    func(e *Event) { e.SourceEpoch = 2 },
		"time":     func(e *Event) { e.OccurredAt = e.OccurredAt.Add(time.Second) },
	} {
		t.Run(name, func(t *testing.T) {
			e := valid()
			mutate(&e)
			if e.Digest() == base {
				t.Fatal("digest ignored a change")
			}
		})
	}
}

func TestOrderIsLexicographicAndStrict(t *testing.T) {
	for _, tc := range []struct {
		next, prior Position
		after       bool
	}{
		{Position{1, 2}, Position{1, 1}, true},
		{Position{1, 1}, Position{1, 1}, false},
		{Position{1, 1}, Position{1, 2}, false},
		{Position{2, 1}, Position{1, 99}, true},
		{Position{1, 99}, Position{2, 1}, false},
	} {
		if tc.next.After(tc.prior) != tc.after {
			t.Fatalf("%+v after %+v", tc.next, tc.prior)
		}
	}
}
