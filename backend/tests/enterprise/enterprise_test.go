package enterprise

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func testDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join("..", "..", "..", ".tools", "fixture-tests")
	if e := os.MkdirAll(root, 0755); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, fmt.Sprintf("%s-%d", strings.ReplaceAll(t.Name(), "/", "-"), time.Now().UnixNano()))
	if e := os.Mkdir(path, 0755); e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := os.RemoveAll(path); e != nil {
			t.Error(e)
		}
	})
	return path
}
func TestDeterministicGenerationAndValidation(t *testing.T) {
	root := testDir(t)
	c := DefaultConfig()
	c.Population = 100
	for _, name := range []string{"one", "two"} {
		out := filepath.Join(root, name)
		if _, e := Generate(c, out); e != nil {
			t.Fatal(e)
		}
		if e := Validate(out); e != nil {
			t.Fatal(e)
		}
	}
	err := filepath.WalkDir(filepath.Join(root, "one"), func(path string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(filepath.Join(root, "one"), path)
		a, e := os.ReadFile(path)
		if e != nil {
			return e
		}
		b, e := os.ReadFile(filepath.Join(root, "two", rel))
		if e != nil {
			return e
		}
		if !bytes.Equal(a, b) {
			return fmt.Errorf("nondeterministic %s", rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, e := Generate(c, filepath.Join(root, "one")); e == nil {
		t.Fatal("overwrote existing dataset")
	}
	// A corrupted chronology is detected independently of generation.
	p := filepath.Join(root, "one", "events/delivery.ndjson")
	data, e := os.ReadFile(p)
	if e != nil {
		t.Fatal(e)
	}
	lines := bytes.Split(bytes.TrimSpace(data), []byte("\n"))
	lines[0], lines[len(lines)-1] = lines[len(lines)-1], lines[0]
	if e = os.WriteFile(p, bytes.Join(lines, []byte("\n")), 0600); e != nil {
		t.Fatal(e)
	}
	if e = Validate(filepath.Join(root, "one")); e == nil {
		t.Fatal("accepted reversed chronology")
	}
}
func TestIndependentAnomalyRates(t *testing.T) {
	c := DefaultConfig()
	c.Population = 10000
	for _, name := range Anomalies {
		t.Run(name, func(t *testing.T) {
			c.Anomalies = map[string]float64{name: 1}
			for i := 0; i < 100; i++ {
				if flags := c.flags(i); !reflect.DeepEqual(flags, []string{name}) {
					t.Fatal(flags)
				}
			}
			c.Anomalies[name] = 0
			if len(c.flags(0)) != 0 {
				t.Fatal("disabled anomaly")
			}
			c.Anomalies[name] = .1
			n := 0
			for i := 0; i < c.Population; i++ {
				if contains(c.flags(i), name) {
					n++
				}
			}
			if n < 800 || n > 1200 {
				t.Fatalf("frequency outside deterministic tolerance: %d", n)
			}
		})
	}
}
func TestAllAnomaliesAreValidTogether(t *testing.T) {
	c := DefaultConfig()
	c.Population = 10
	c.Cohorts = map[string]float64{"multichannel": 1}
	for _, name := range Anomalies {
		c.Anomalies[name] = 1
	}
	out := filepath.Join(testDir(t), "all")
	s, e := Generate(c, out)
	if e != nil {
		t.Fatal(e)
	}
	if e = Validate(out); e != nil {
		t.Fatal(e)
	}
	for _, name := range Anomalies {
		if s.Anomalies[name] != 10 {
			t.Fatal(name)
		}
	}
	var kinds = map[string]bool{}
	e = readLines(filepath.Join(out, "events/occurrence.ndjson"), func(ev Event, _ int) error { kinds[ev.Kind] = true; return nil })
	if e != nil {
		t.Fatal(e)
	}
	for _, name := range []string{"delete", "recreate", "consent_withdrawal", "late_event", "out_of_order", "backfill", "partial_batch", "rejected_import", "source_owner_change", "identifier_correction", "migration_cutover", "retention_review"} {
		if !kinds[name] {
			t.Fatal("missing scenario effect", name)
		}
	}
}
func TestCohortIsolationAndSeed(t *testing.T) {
	c := DefaultConfig()
	c.Anomalies = map[string]float64{}
	for cohort, source := range map[string]string{"crm_only": "salesforce", "ecommerce_only": "shopify", "dormant_erp": "erp"} {
		c.Cohorts = map[string]float64{cohort: 1}
		for i := 0; i < 100; i++ {
			if got := sourceSet(c.cohort(i), i, c); !reflect.DeepEqual(got, []string{source}) {
				t.Fatal(got)
			}
		}
	}
	c = DefaultConfig()
	other := c
	other.Seed++
	if c.draw(1, "cohort") == other.draw(1, "cohort") {
		t.Fatal("seed ignored")
	}
	c.Anomalies["typo"] = .1
	if c.Validate() == nil {
		t.Fatal("unknown anomaly accepted")
	}
}
func TestNormalizationAndNullSemantics(t *testing.T) {
	for _, source := range Sources {
		m, e := NormalizeSnapshot(source, Sample(source))
		if source == "identity" {
			if e == nil {
				t.Fatal("pretended identity connector")
			}
			continue
		}
		if e != nil {
			t.Fatal(source, e)
		}
		if len(m.Identifiers) != 1 || m.Identifiers[0].Namespace != source {
			t.Fatal(m)
		}
	}
	cdc := []byte(`{"data":{"payload":{"ChangeEventHeader":{"entityName":"Contact","changeType":"UPDATE","recordIds":["003000000000020"]},"Email":null,"LastName":"Renamed"}}}`)
	id, m, e := NormalizeContactCDC(cdc)
	if e != nil || id != "003000000000020" || !reflect.DeepEqual(m.Remove, []string{"email"}) || string(m.Set["display_name"]) != `"Renamed"` {
		t.Fatal(id, m, e)
	}
	if _, ok := m.Set["phone"]; ok {
		t.Fatal("unchanged field overwritten")
	}
	raw := bytes.ReplaceAll(Sample("salesforce"), []byte(`"LastName":"Exemple"`), []byte(`"LastName":[]`))
	if _, e = NormalizeSnapshot("salesforce", raw); e == nil {
		t.Fatal("malformed data accepted")
	}
	raw = bytes.ReplaceAll(Sample("braze"), []byte(`"external_id":"retail-00000002",`), nil)
	if _, e = NormalizeSnapshot("braze", raw); e == nil {
		t.Fatal("anonymous record linked")
	}
	if _, e = ExplicitTarget([]string{"a", "b"}, ""); e == nil {
		t.Fatal("ambiguous evidence selected a target")
	}
	if _, e = ExplicitTarget([]string{"a"}, "missing"); e == nil {
		t.Fatal("missing reference accepted")
	}
}
func TestSeparateMatchingMetrics(t *testing.T) {
	truth := []Link{{"a", "b", "verified", ""}, {"c", "d", "unsafe", ""}, {"e", "f", "verified", ""}}
	s := Evaluate(truth, [][2]string{{"a", "b"}, {"c", "d"}, {"d", "c"}})
	if s.FalsePositiveMerges != 1 || s.MissedMatches != 1 {
		t.Fatal(s)
	}
}
func TestReplayAndCurated(t *testing.T) {
	root := testDir(t)
	out := filepath.Join(root, "curated")
	if e := WriteCurated(out); e != nil {
		t.Fatal(e)
	}
	data, e := os.ReadFile(filepath.Join(out, "sources/salesforce/cdc-update-null.json"))
	if e != nil {
		t.Fatal(e)
	}
	if _, _, e = NormalizeContactCDC(data); e != nil {
		t.Fatal(e)
	}
	var scenarios []Scenario
	data, e = os.ReadFile(filepath.Join(out, "scenarios.json"))
	if e != nil {
		t.Fatal(e)
	}
	if e = json.Unmarshal(data, &scenarios); e != nil || len(scenarios) != len(Scenarios()) {
		t.Fatal(e)
	}
	c := DefaultConfig()
	c.Population = 10
	out = filepath.Join(root, "dataset")
	if _, e = Generate(c, out); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(out, "events/delivery.ndjson")
	var a, b bytes.Buffer
	if e = ReplayShuffled(path, &a, 42); e != nil {
		t.Fatal(e)
	}
	if e = ReplayShuffled(path, &b, 42); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(a.Bytes(), b.Bytes()) {
		t.Fatal("shuffle nondeterministic")
	}
	original, e := os.ReadFile(path)
	if e != nil {
		t.Fatal(e)
	}
	if bytes.Equal(original, a.Bytes()) {
		t.Fatal("shuffle unchanged")
	}
}

func TestPrivacyAndIdentityConflictFixtures(t *testing.T) {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	flags := []string{"consent_lag", "shared_email", "reused_identifier"}
	shop := rawRecord("shopify", recordID("shopify", 2, 0), personAt(2), flags, at, at)
	braze := rawRecord("braze", recordID("braze", 2, 0), personAt(2), flags, at, at)
	customer := shop["data"].(object)["customers"].(object)["nodes"].([]object)[0]
	if customer["defaultEmailAddress"].(object)["marketingState"] != "UNSUBSCRIBED" || braze["users"].([]object)[0]["email_subscribe"] != "opted_in" {
		t.Fatal("consent lag missing")
	}
	a, _ := json.Marshal(rawRecord("salesforce", recordID("salesforce", 2, 0), personAt(2), flags, at, at))
	b, _ := json.Marshal(rawRecord("salesforce", recordID("salesforce", 3, 0), personAt(3), flags, at, at))
	ma, err := NormalizeSnapshot("salesforce", a)
	if err != nil {
		t.Fatal(err)
	}
	mb, err := NormalizeSnapshot("salesforce", b)
	if err != nil {
		t.Fatal(err)
	}
	if string(ma.Set["email"]) != string(mb.Set["email"]) || ma.Identifiers[0].Value == mb.Identifiers[0].Value {
		t.Fatal("shared-email different-person scenario missing")
	}
	for key := range ma.Set {
		if key != "email" && key != "display_name" {
			t.Fatal("normalizer expanded authority", key)
		}
	}
	for _, source := range []string{"salesforce", "braze"} {
		data, _ := json.Marshal(rawRecord(source, recordID(source, 2, 0), personAt(2), []string{"deleted_downstream"}, at, at))
		_, err := NormalizeSnapshot(source, data)
		if (source == "salesforce") != (err != nil) {
			t.Fatal("deleted upstream/downstream retention scenario", source, err)
		}
	}
}
