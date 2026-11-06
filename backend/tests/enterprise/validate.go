package enterprise

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"
)

func readLines[T any](path string, fn func(T, int) error) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 65536), 2<<20)
	n := 0
	for scanner.Scan() {
		n++
		var v T
		if e = json.Unmarshal(scanner.Bytes(), &v); e != nil {
			return fmt.Errorf("%s:%d: %w", path, n, e)
		}
		if e = fn(v, n); e != nil {
			return fmt.Errorf("%s:%d: %w", path, n, e)
		}
	}
	return scanner.Err()
}
func Validate(out string) error {
	data, e := os.ReadFile(filepath.Join(out, "manifest.json"))
	if e != nil {
		return e
	}
	var stats Stats
	if e = json.Unmarshal(data, &stats); e != nil {
		return e
	}
	if e = stats.Config.Validate(); e != nil {
		return e
	}
	people := map[string]Person{}
	cohorts := map[string]int{}
	anomalies := map[string]int{}
	e = readLines(filepath.Join(out, "expected/population.ndjson"), func(p Person, line int) error {
		if _, ok := people[p.ID]; ok {
			return fmt.Errorf("duplicate person")
		}
		if p.ID != fmt.Sprintf("truth-%06d", line-1) {
			return fmt.Errorf("population order")
		}
		if p.Cohort != stats.Config.cohort(line-1) || !reflect.DeepEqual(p.Flags, stats.Config.flags(line-1)) {
			return fmt.Errorf("configured distributions not reproduced")
		}
		people[p.ID] = p
		cohorts[p.Cohort]++
		for _, f := range p.Flags {
			anomalies[f]++
		}
		return nil
	})
	if e != nil {
		return e
	}
	if len(people) != stats.People || stats.People != stats.Config.Population || !reflect.DeepEqual(cohorts, stats.Cohorts) || !reflect.DeepEqual(anomalies, stats.Anomalies) {
		return fmt.Errorf("population/statistics mismatch")
	}
	records := map[string]TruthRecord{}
	e = readLines(filepath.Join(out, "expected/records.ndjson"), func(r TruthRecord, _ int) error {
		if _, ok := records[r.Ref]; ok {
			return fmt.Errorf("duplicate reference")
		}
		if r.Person != "" {
			if _, ok := people[r.Person]; !ok {
				return fmt.Errorf("unknown person")
			}
		} else if r.LinkClass != "unknown" {
			return fmt.Errorf("unknown person requires unknown classification")
		}
		records[r.Ref] = r
		return nil
	})
	if e != nil {
		return e
	}
	total := 0
	for _, source := range Sources {
		n := 0
		e = readLines(filepath.Join(out, "sources", source, "records.ndjson"), func(raw json.RawMessage, line int) error {
			n++
			ref := fmt.Sprintf("sources/%s/records.ndjson#%d", source, line)
			r, ok := records[ref]
			if !ok || r.Source != source {
				return fmt.Errorf("missing truth reference")
			}
			if strings.Contains(string(raw), "truth-") {
				return fmt.Errorf("ground truth leaked into source")
			}
			if r.DuplicateOf != "" {
				other, ok := records[r.DuplicateOf]
				if !ok || other.Person != r.Person || other.Source != r.Source {
					return fmt.Errorf("invalid duplicate link")
				}
			}
			return nil
		})
		if e != nil {
			return e
		}
		if n != stats.Records[source] {
			return fmt.Errorf("source count mismatch")
		}
		total += n
	}
	if total != len(records) {
		return fmt.Errorf("orphan truth records")
	}
	e = readLines(filepath.Join(out, "expected/links.ndjson"), func(l Link, _ int) error {
		a, aok := records[l.Left]
		b, bok := records[l.Right]
		if !aok || !bok {
			return fmt.Errorf("missing link endpoint")
		}
		switch l.Class {
		case "verified", "ambiguous", "duplicate_source":
			if a.Person == "" || a.Person != b.Person {
				return fmt.Errorf("invalid same-person link")
			}
		case "different", "unsafe":
			if a.Person == "" || b.Person == "" || a.Person == b.Person {
				return fmt.Errorf("invalid different-person link")
			}
		default:
			return fmt.Errorf("unknown link class")
		}
		return nil
	})
	if e != nil {
		return e
	}
	refdate, _ := time.Parse("2006-01-02", stats.Config.ReferenceDate)
	var previous time.Time
	deliveries := map[string][32]byte{}
	unique := map[string][32]byte{}
	e = readLines(filepath.Join(out, "events/occurrence.ndjson"), func(ev Event, _ int) error {
		if _, ok := records[ev.Ref]; !ok {
			return fmt.Errorf("event refers to missing source record")
		}
		if ev.Source != records[ev.Ref].Source {
			return fmt.Errorf("event source mismatch")
		}
		if ev.Occurred.Before(previous) || ev.Delivered.Before(ev.Occurred) || ev.Delivered.After(refdate) || ev.Occurred.Year() < 2011 {
			return fmt.Errorf("invalid event chronology")
		}
		previous = ev.Occurred
		if _, ok := deliveries[ev.DeliveryID]; ok {
			return fmt.Errorf("duplicate delivery identifier")
		}
		raw, _ := json.Marshal(ev)
		deliveries[ev.DeliveryID] = sha256.Sum256(raw)
		normalized := ev
		normalized.DeliveryID = ""
		normalized.Delivered = time.Time{}
		raw, _ = json.Marshal(normalized)
		signature := sha256.Sum256(raw)
		if before, ok := unique[ev.ID]; ok && before != signature {
			return fmt.Errorf("duplicate event changed intent")
		}
		unique[ev.ID] = signature
		return nil
	})
	if e != nil {
		return e
	}
	if len(deliveries) != stats.Events {
		return fmt.Errorf("event count mismatch")
	}
	previous = time.Time{}
	n := 0
	e = readLines(filepath.Join(out, "events/delivery.ndjson"), func(ev Event, _ int) error {
		n++
		if ev.Delivered.Before(previous) {
			return fmt.Errorf("delivery order")
		}
		previous = ev.Delivered
		original, ok := deliveries[ev.DeliveryID]
		if !ok || original != eventDigest(ev) {
			return fmt.Errorf("delivery differs from event")
		}
		delete(deliveries, ev.DeliveryID)
		return nil
	})
	if e != nil {
		return e
	}
	if len(deliveries) != 0 || n != stats.Events {
		return fmt.Errorf("missing deliveries")
	}
	return nil
}

func eventDigest(ev Event) [32]byte { raw, _ := json.Marshal(ev); return sha256.Sum256(raw) }
