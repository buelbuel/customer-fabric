package enterprise

import (
	"bufio"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type TruthRecord struct {
	Ref         string   `json:"ref"`
	Source      string   `json:"source"`
	RecordID    string   `json:"record_id"`
	Person      string   `json:"person,omitempty"`
	Cohort      string   `json:"cohort"`
	Flags       []string `json:"anomalies"`
	DuplicateOf string   `json:"duplicate_of,omitempty"`
	LinkClass   string   `json:"link_class"`
}
type Person struct {
	ID     string   `json:"id"`
	Cohort string   `json:"cohort"`
	Flags  []string `json:"anomalies"`
}
type Link struct {
	Left   string `json:"left"`
	Right  string `json:"right"`
	Class  string `json:"class"`
	Reason string `json:"reason"`
}
type Event struct {
	ID         string    `json:"event_id"`
	DeliveryID string    `json:"delivery_id"`
	Ref        string    `json:"record_ref"`
	Source     string    `json:"source"`
	Kind       string    `json:"kind"`
	Occurred   time.Time `json:"occurred_at"`
	Delivered  time.Time `json:"delivered_at"`
	Sequence   int       `json:"source_sequence"`
	Changes    object    `json:"changes,omitempty"`
}
type Stats struct {
	Config    Config         `json:"config"`
	People    int            `json:"people"`
	Records   map[string]int `json:"records"`
	Cohorts   map[string]int `json:"cohorts"`
	Anomalies map[string]int `json:"anomalies"`
	Events    int            `json:"events"`
}
type writer struct {
	f *os.File
	b *bufio.Writer
	e *json.Encoder
}

func newWriter(path string) (*writer, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	b := bufio.NewWriter(f)
	return &writer{f, b, json.NewEncoder(b)}, nil
}
func (w *writer) close() error {
	err := w.b.Flush()
	e := w.f.Close()
	if err != nil {
		return err
	}
	return e
}
func idFor(parts string) string { h := sha256.Sum256([]byte(parts)); return hex.EncodeToString(h[:16]) }

// Generate never contacts a database, overwrites a directory, or opens expected data for ingestion.
// Source snapshots and events stream to disk. Event sorting uses bounded persistent blocks.
func Generate(c Config, out string) (stats Stats, err error) {
	if err = c.Validate(); err != nil {
		return stats, err
	}
	if err = os.Mkdir(out, 0755); err != nil {
		return stats, fmt.Errorf("output must not exist: %w", err)
	}
	stats = Stats{Config: c, People: c.Population, Records: map[string]int{}, Cohorts: map[string]int{}, Anomalies: map[string]int{}}
	var writers []*writer
	open := func(name string) (*writer, error) {
		w, e := newWriter(filepath.Join(out, name))
		if e == nil {
			writers = append(writers, w)
		}
		return w, e
	}
	defer func() {
		for _, w := range writers {
			if e := w.close(); err == nil {
				err = e
			}
		}
	}()
	raw := map[string]*writer{}
	for _, s := range Sources {
		w, e := open("sources/" + s + "/records.ndjson")
		if e != nil {
			return stats, e
		}
		raw[s] = w
	}
	people, e := open("expected/population.ndjson")
	if e != nil {
		return stats, e
	}
	truth, e := open("expected/records.ndjson")
	if e != nil {
		return stats, e
	}
	links, e := open("expected/links.ndjson")
	if e != nil {
		return stats, e
	}
	outcomes, e := open("expected/outcomes.ndjson")
	if e != nil {
		return stats, e
	}
	csvout, e := open("sources/erp/historical.csv")
	if e != nil {
		return stats, e
	}
	cw := csv.NewWriter(csvout.b)
	cw.Comma = ';'
	if e = cw.Write([]string{"KUNDENNR", "VORNAME", "NACHNAME", "LAND", "ANGELEGT", "EMAIL"}); e != nil {
		return stats, e
	}
	refdate, _ := time.Parse("2006-01-02", c.ReferenceDate)
	spool, e := open("events/by-record.ndjson")
	if e != nil {
		return stats, e
	}
	var events []Event
	firstRef := make(map[int]string)
	evidence := map[string]TruthRecord{}
	for i := 0; i < c.Population; i++ {
		cohort := c.cohort(i)
		flags := c.flags(i)
		stats.Cohorts[cohort]++
		for _, f := range flags {
			stats.Anomalies[f]++
		}
		pid := fmt.Sprintf("truth-%06d", i)
		if e = people.e.Encode(Person{pid, cohort, flags}); e != nil {
			return stats, e
		}
		sources := sourceSet(cohort, i, c)
		var same []string
		for _, source := range sources {
			copies := 1
			if contains(flags, "duplicate_source") {
				copies = 2
			}
			for d := 0; d < copies; d++ {
				id := recordID(source, i, d)
				stats.Records[source]++
				line := stats.Records[source]
				ref := fmt.Sprintf("sources/%s/records.ndjson#%d", source, line)
				class := "verified"
				personID := pid
				if cohort == "fragmented" {
					class = "ambiguous"
				}
				if len(sources) == 1 {
					class = "source_only"
				}
				if contains(flags, "anonymous") && source == "braze" {
					personID = ""
					class = "unknown"
				}
				tr := TruthRecord{ref, source, id, personID, cohort, flags, "", class}
				if d > 0 {
					tr.DuplicateOf = fmt.Sprintf("sources/%s/records.ndjson#%d", source, line-1)
				}
				events = events[:0]
				if tr.DuplicateOf != "" && tr.Person != "" {
					if e = links.e.Encode(Link{tr.DuplicateOf, tr.Ref, "duplicate_source", "separate records, same individual"}); e != nil {
						return stats, e
					}
				}
				if e = truth.e.Encode(tr); e != nil {
					return stats, e
				}
				if d == 0 && personID != "" {
					same = append(same, ref)
					if _, ok := firstRef[i]; !ok {
						firstRef[i] = ref
					}
				}
				start := time.Date(launch[source], 1, 1, 0, 0, 0, 0, time.UTC)
				// The early ERP cohort anchors 2011; timestamps span through the fixed 2026 reference.
				created := start.Add(time.Duration(c.draw(i, "created/"+source) * float64(refdate.Sub(start)/2)))
				if line == 1 && source == "erp" {
					created = start
				}
				updated := refdate.Add(-time.Duration(10+i%80) * 24 * time.Hour)
				if cohort == "dormant_erp" {
					created = start.Add(time.Duration(c.draw(i, "dormant-created") * float64(4*365*24*time.Hour)))
					updated = created.AddDate(0, 3, 0)
				}
				payload := rawRecord(source, id, personAt(i), flags, created, updated)
				if source == "erp" && cohort == "dormant_erp" {
					payload["AKTIV"] = false
				}
				if source == "braze" && !contains(flags, "missing_reference") {
					attributes := payload["users"].([]object)[0]["custom_attributes"].(object)
					if !contains(sources, "salesforce") {
						delete(attributes, "crm_contact_id")
					} else if i%5 == 0 {
						delete(attributes, "crm_contact_id")
						attributes["crm_lead_id"] = recordID("salesforce", i, 0)
					}
				}
				if contains(flags, "malformed") {
					malform(source, payload)
				}
				if personID != "" {
					data, _ := json.Marshal(payload)
					if normalized, normalizeErr := NormalizeSnapshot(source, data); normalizeErr == nil {
						keys := []string{}
						if email, ok := normalized.Set["email"]; ok {
							keys = append(keys, "email/"+string(email))
						}
						for _, identifier := range normalized.Identifiers {
							keys = append(keys, "identifier/"+source+"/"+identifier.Value)
						}
						for _, key := range keys {
							if other, ok := evidence[key]; ok && other.Person != personID {
								if e = links.e.Encode(Link{other.Ref, ref, "unsafe", "shared observed evidence is not permission to merge"}); e != nil {
									return stats, e
								}
							} else if !ok {
								evidence[key] = tr
							}
						}
					}
				}
				if e = raw[source].e.Encode(payload); e != nil {
					return stats, e
				}
				if source == "erp" {
					p := personAt(i)
					if e = cw.Write([]string{id, p.first, p.last, fmt.Sprint(payload["LAND"]), fmt.Sprint(payload["ANGELEGT"]), fmt.Sprint(payload["EMAIL"])}); e != nil {
						return stats, e
					}
				}
				seq := 0
				add := func(kind string, at time.Time, changes object) Event {
					seq++
					eid := idFor(fmt.Sprintf("%d/%s/%s/%d", c.Seed, ref, kind, seq))
					ev := Event{eid, eid + "-1", ref, source, kind, at, at.Add(time.Minute), seq, changes}
					events = append(events, ev)
					return ev
				}
				add("initial_import", created, object{"snapshot_ref": ref})
				if source == "loyalty" {
					kind := "loyalty_enrollment"
					if d > 0 {
						kind = "duplicate_enrollment_attempt"
					}
					add(kind, created, object{"membership_id": id})
					if contains(flags, "inactive") {
						add("membership_status_change", updated, object{"status": "suspended"})
					}
				}
				add("full_import", created.AddDate(1, 0, 0), object{"snapshot_ref": ref})
				update := add("incremental_update", updated, object{"display_name": personAt(i).first + " " + personAt(i).last})
				if cohort == "dormant_erp" {
					add("retention_review", refdate.Add(-24*time.Hour), object{"reason": "dormant_erp_cohort", "automatic_erasure": false})
				}
				for _, f := range flags {
					at := updated.Add(-24 * time.Hour)
					switch f {
					case "duplicate_event", "retry_storm":
						count := 1
						if f == "retry_storm" {
							count = 5
						}
						for k := 0; k < count; k++ {
							dup := update
							dup.DeliveryID = update.ID + fmt.Sprintf("-%s-%d", f, k)
							dup.Delivered = update.Delivered.Add(time.Duration(k+1) * time.Hour)
							events = append(events, dup)
						}
					case "late_event", "out_of_order", "backfill", "downtime":
						old := created.AddDate(0, 1, 0)
						ev := add(f, old, object{"display_name": "Historical synthetic value"})
						events[len(events)-1].Delivered = refdate.Add(-24 * time.Hour)
						_ = ev
					case "partial_migration", "partial_batch":
						add(f, at, object{"batch_id": "synthetic-migration-2020", "accepted_rows": 8, "rejected_rows": 2, "complete": false})
					case "deleted_downstream":
						add("delete", at, object{"downstream_copy_remains": true})
					case "recreated":
						add("delete", at.Add(-time.Hour), nil)
						add("recreate", at, object{"new_external_id": id + "-recreated"})
					case "consent_lag":
						add("consent_withdrawal", at, object{"purpose": "email_marketing", "state": "withdrawn", "downstream_state": "subscribed", "restriction": "suppress_email_marketing"})
					case "owner_change":
						add("source_owner_change", at, object{"from": "erp", "to": "salesforce", "m0": "ownership_transfer_unsupported"})
					case "identifier_correction":
						add(f, at, object{"previous": id + "-incorrect", "corrected": id})
					case "missing_reference":
						add(f, at, object{"referenced_record": "missing-record"})
					case "malformed":
						add("rejected_import", at, object{"email": []string{"bad"}, "reason": "wrong_field_type"})
					case "address_change":
						if source == "shopify" {
							add("address_change", at, object{"address1": "Synthetic New Street 0", "previous_address1": "Synthetic Test Street 0"})
						}
					case "loyalty_delay":
						if source == "loyalty" {
							add("identifier_propagation", at, object{"customer_reference": fmt.Sprintf("retail-%08d", i)})
							events[len(events)-1].Delivered = refdate.Add(-24 * time.Hour)
						}
					case "name_change":
						add(f, at, object{"previous_last_name": personAt(i).last, "last_name": personAt(i).last + "-Renamed"})
					case "phone_change":
						add(f, at, object{"previous_phone": personAt(i).phone, "phone": fmt.Sprintf("01632 960%03d", (i+17)%1000)})
					case "inactive":
						add("retention_review", at, object{"reason": "inactive", "policy": "synthetic-controller-policy", "automatic_erasure": false})
					case "schema_variation":
						add("migration_cutover", created.AddDate(0, 6, 0), object{"from_schema": "historical", "to_schema": "current"})
					}
				}
				sort.Slice(events, func(i, j int) bool {
					if !events[i].Occurred.Equal(events[j].Occurred) {
						return events[i].Occurred.Before(events[j].Occurred)
					}
					return events[i].DeliveryID < events[j].DeliveryID
				})
				sequences := map[string]int{}
				next := 0
				for _, ev := range events {
					seq, ok := sequences[ev.ID]
					if !ok {
						next++
						seq = next
						sequences[ev.ID] = seq
					}
					ev.Sequence = seq
					if e = spool.e.Encode(ev); e != nil {
						return stats, e
					}
				}
				stats.Events += len(events)
				restrictions := []string{}
				if contains(flags, "consent_lag") {
					restrictions = append(restrictions, "suppress_email_marketing")
				}
				if contains(flags, "deleted_downstream") {
					restrictions = append(restrictions, "erase_downstream_copy")
				}
				if e = outcomes.e.Encode(object{"record_ref": ref, "ownership_conflict": contains(flags, "conflicting_values"), "rejected_import": normalizationRejected(source, payload), "missing_reference": contains(flags, "missing_reference"), "restrictions": restrictions}); e != nil {
					return stats, e
				}
			}
		}
		for j := 1; j < len(same); j++ {
			class := "verified"
			if cohort == "fragmented" {
				class = "ambiguous"
			}
			if e = links.e.Encode(Link{same[0], same[j], class, "same synthetic individual; verification is an explicit scenario assumption"}); e != nil {
				return stats, e
			}
		}
	}
	cw.Flush()
	if e = cw.Error(); e != nil {
		return stats, e
	}
	for i := 1; i < c.Population; i++ {
		if firstRef[i-1] == "" || firstRef[i] == "" {
			continue
		}
		class := "different"
		reason := "different synthetic individuals"
		if e = links.e.Encode(Link{firstRef[i-1], firstRef[i], class, reason}); e != nil {
			return stats, e
		}
	}
	if e = spool.b.Flush(); e != nil {
		return stats, e
	}
	for _, order := range []string{"occurrence", "delivery"} {
		if e = sortEventFile(filepath.Join(out, "events/by-record.ndjson"), filepath.Join(out, "events"), order); e != nil {
			return stats, e
		}
	}
	summary, e := open("manifest.json")
	if e != nil {
		return stats, e
	}
	summary.e.SetIndent("", "  ")
	err = summary.e.Encode(stats)
	return stats, err
}

// Replay streams existing delivery order; it has no HTTP or database capabilities.
func Replay(path string, w io.Writer) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	_, e = io.Copy(w, f)
	return e
}
