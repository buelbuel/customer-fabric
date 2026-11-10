package enterprise

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/matching"
)

// matchAttributes maps a raw synthetic source record onto the attribute paths matching reads. It stands
// in for each connector's mapping; a real source writes these attributes itself.
func matchAttributes(source string, raw []byte) map[string]json.RawMessage {
	var root object
	if json.Unmarshal(raw, &root) != nil {
		return nil
	}
	field := func(o object, key string) any { return o[key] }
	pick := map[string]any{}
	switch source {
	case "salesforce":
		pick = map[string]any{"email": root["Email"], "phone": root["Phone"], "given_name": root["FirstName"], "family_name": root["LastName"]}
	case "braze":
		if users, ok := root["users"].([]any); ok && len(users) == 1 {
			u, _ := users[0].(map[string]any)
			pick = map[string]any{"email": u["email"], "phone": u["phone"], "given_name": u["first_name"], "family_name": u["last_name"], "customer_reference": u["external_id"]}
		}
	case "shopify":
		if data, ok := root["data"].(map[string]any); ok {
			nodes, _ := field(data["customers"].(map[string]any), "nodes").([]any)
			if len(nodes) == 1 {
				u, _ := nodes[0].(map[string]any)
				pick = map[string]any{"given_name": u["firstName"], "family_name": u["lastName"]}
				if e, ok := u["defaultEmailAddress"].(map[string]any); ok {
					pick["email"] = e["emailAddress"]
				}
				if p, ok := u["defaultPhoneNumber"].(map[string]any); ok {
					pick["phone"] = p["phoneNumber"]
				}
			}
		}
	case "emarsys":
		if contacts, ok := root["contacts"].([]any); ok && len(contacts) == 1 {
			u, _ := contacts[0].(map[string]any)
			pick = map[string]any{"given_name": u["1"], "family_name": u["2"], "email": u["3"], "customer_reference": u["9001"]}
		}
	case "erp":
		pick = map[string]any{"email": root["EMAIL"], "phone": root["TELEFON"], "given_name": root["VORNAME"], "family_name": root["NACHNAME"]}
	case "loyalty":
		pick = map[string]any{"email": root["email"], "customer_reference": root["customer_reference"]}
	case "identity":
		pick = map[string]any{"email": root["verified_email"], "customer_reference": root["retail_reference"]}
	}
	out := map[string]json.RawMessage{}
	for path, value := range pick {
		if s, ok := value.(string); ok && s != "" {
			out[path], _ = json.Marshal(s)
		}
	}
	return out
}

// Candidate matching is measured against the generator's hidden truth, which matching never sees:
// proposals must be precise, and no pair of different people that the data could tell apart (by a name or
// phone) may be proposed. Recall is pairwise, before any transitive grouping.
func TestMatchCandidatesAgainstHiddenTruth(t *testing.T) {
	c := DefaultConfig()
	c.Population = 400
	for _, anomaly := range []string{"shared_email", "reused_identifier", "name_change", "phone_change", "multiple_emails", "duplicate_source"} {
		c.Anomalies[anomaly] = 0.15
	}
	out := filepath.Join(testDir(t), "matching")
	if _, err := Generate(c, out); err != nil {
		t.Fatal(err)
	}
	truth := map[string]string{}
	if err := readLines(filepath.Join(out, "expected", "records.ndjson"), func(r struct{ Ref, Person string }, _ int) error {
		truth[r.Ref] = r.Person
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	type record struct {
		ref     string
		signals matching.Signals
	}
	var records []record
	sources, err := os.ReadDir(filepath.Join(out, "sources"))
	if err != nil {
		t.Fatal(err)
	}
	for _, source := range sources {
		if err = readLines(filepath.Join(out, "sources", source.Name(), "records.ndjson"), func(raw json.RawMessage, line int) error {
			ref := fmt.Sprintf("sources/%s/records.ndjson#%d", source.Name(), line)
			if _, known := truth[ref]; !known {
				return fmt.Errorf("%s has no hidden truth", ref)
			}
			records = append(records, record{ref, matching.SignalsFrom(matchAttributes(source.Name(), raw))})
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	var truePositive, falsePositive, falseNegative, wrong, undecidable int
	classes := map[string]int{}
	for i := range records {
		for j := i + 1; j < len(records); j++ {
			same := truth[records[i].ref] == truth[records[j].ref]
			candidate, ok := matching.Compare(records[i].signals, records[j].signals)
			if ok {
				classes[candidate.Class]++
			}
			proposed := ok && candidate.Class != matching.Conflicting
			switch {
			case proposed && same:
				truePositive++
			case proposed:
				falsePositive++
				// A pair is undecidable when one side carries nothing beyond the shared identifiers: no name or
				// phone could contradict the copy. Every other wrong proposal is a rule defect.
				bare := func(s matching.Signals) bool { return s.GivenName == "" && s.FamilyName == "" && s.Phone == "" }
				if bare(records[i].signals) || bare(records[j].signals) {
					undecidable++
				} else {
					wrong++
					t.Logf("wrong proposal %s | %s | %+v", records[i].ref, records[j].ref, candidate)
				}
			case same:
				falseNegative++
			}
		}
	}
	precision := float64(truePositive) / float64(truePositive+falsePositive)
	recall := float64(truePositive) / float64(truePositive+falseNegative)
	t.Logf("%d records: precision %.3f, recall %.3f, undecidable false positives %d, classes %v", len(records), precision, recall, undecidable, classes)
	if wrong != 0 {
		t.Fatalf("%d pairs of different people with distinguishing signals were proposed", wrong)
	}
	// Copied identifiers against a record with nothing else are indistinguishable from the same person; they
	// stay counted as false positives and must remain rare.
	// The fixture is deterministic, so the absolute count is pinned too and a regression cannot hide in scale.
	if proposals := truePositive + falsePositive; undecidable > 2 || undecidable*200 > proposals {
		t.Fatalf("%d undecidable false positives among %d proposals", undecidable, proposals)
	}
	if len(records) != len(truth) {
		t.Fatalf("matched %d of %d records with hidden truth", len(records), len(truth))
	}
	if precision < 0.99 || recall < 0.6 {
		t.Fatalf("precision %.3f, recall %.3f", precision, recall)
	}
	if classes[matching.Conflicting] == 0 {
		t.Fatal("the adversarial cases produced no conflicting candidates to review")
	}
}
