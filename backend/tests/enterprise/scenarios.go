package enterprise

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type Scenario struct {
	ID        string `json:"id"`
	Layer     string `json:"layer"`
	Expected  string `json:"expected"`
	Supported bool   `json:"m0_supported"`
}

func Scenarios() []Scenario {
	return []Scenario{
		{"explicit_identifier_link", "api", "caller-selected profile receives source-scoped identifier", true},
		{"ambiguous_matching", "adapter", "no target without explicit verified link; no matching algorithm exists", false},
		{"ownership_conflict", "api", "409; provenance rejection; no state or outbox change", true},
		{"duplicate_delivery", "api", "unchanged source/key returns original receipt", true},
		{"out_of_order", "api", "412 for stale If-Match; event-time ordering is not implemented", true},
		{"idempotent_replay", "api", "stable receipt even after newer accepted mutations", true},
		{"missing_reference", "adapter", "quarantine; never invent a target or follow stale CRM pointer", false},
		{"consent_conflict", "fixture", "suppress email marketing in expected policy; M0 has no consent enforcement", false},
		{"historical_backfill", "api", "caller-controlled current version required; historical time is not authority", true},
		{"partial_import", "api", "each request atomic; failed item does not undo other successful items", true},
		{"erasure_replay", "fixture", "future epoch fence must reject resurrection; M0 erasure unsupported", false},
		{"malformed_import", "adapter", "reject malformed scalar data before API submission", true},
		{"cdc_null", "adapter", "CometD null removes field; absent field remains untouched", true},
		{"lead_conversion", "fixture", "Contact/Lead/Account references are evidence, not automatic merge instructions", false},
	}
}

// ExplicitTarget is fixture-driver policy, not a production authorization or matching service.
// The verified target must be supplied independently of candidate evidence and ground truth.
func ExplicitTarget(candidates []string, verified string) (string, error) {
	if verified == "" {
		return "", fmt.Errorf("quarantine: explicit verified link required")
	}
	for _, candidate := range candidates {
		if candidate == verified {
			return verified, nil
		}
	}
	return "", fmt.Errorf("verified target not in candidate set")
}
func Sample(source string) []byte {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b, _ := json.Marshal(rawRecord(source, recordID(source, 2, 0), personAt(2), nil, at, at))
	return b
}

// WriteCurated emits standalone source examples, never a live-vendor capture.
func WriteCurated(out string) error {
	if err := os.Mkdir(out, 0755); err != nil {
		return err
	}
	write := func(name string, v any) error {
		b, e := json.MarshalIndent(v, "", "  ")
		if e != nil {
			return e
		}
		path := filepath.Join(out, name)
		if e = os.MkdirAll(filepath.Dir(path), 0755); e != nil {
			return e
		}
		return os.WriteFile(path, append(b, '\n'), 0600)
	}
	for _, s := range Sources {
		var raw any
		if e := json.Unmarshal(Sample(s), &raw); e != nil {
			return e
		}
		if e := write("sources/"+s+"/sample.json", raw); e != nil {
			return e
		}
	}
	if e := write("scenarios.json", Scenarios()); e != nil {
		return e
	}
	// CometD body presence is authoritative; no Pub/Sub-only nulledFields/diffFields.
	cdc := func(kind string, payload object, n int) object {
		payload["ChangeEventHeader"] = object{"entityName": "Contact", "recordIds": []string{sfid("003", 20)}, "changeType": kind, "changeOrigin": "com/salesforce/api/rest/65.0;client=synthetic-fixtures", "transactionKey": fmt.Sprintf("synthetic-transaction-%d", n), "sequenceNumber": 1, "commitTimestamp": 1767225600000 + int64(n)*1000, "commitNumber": n, "commitUser": sfid("005", 1)}
		return object{"channel": "/data/ContactChangeEvent", "data": object{"schema": "synthetic-schema-id", "event": object{"replayId": n}, "payload": payload}}
	}
	for _, item := range []struct {
		name, kind string
		payload    object
		n          int
	}{{"create", "CREATE", object{"LastName": "Exemple", "Email": "person-000002@example.com"}, 1}, {"update-null", "UPDATE", object{"Email": nil, "LastName": "Exemple-Renamed"}, 2}, {"owner-change", "UPDATE", object{"OwnerId": sfid("005", 2)}, 3}, {"delete", "DELETE", object{}, 4}, {"undelete", "UNDELETE", object{"LastName": "Exemple"}, 5}} {
		if e := write("sources/salesforce/cdc-"+item.name+".json", cdc(item.kind, item.payload, item.n)); e != nil {
			return e
		}
	}
	lead := object{"attributes": object{"type": "Lead", "url": "/services/data/v65.0/sobjects/Lead/" + sfid("00Q", 20)}, "Id": sfid("00Q", 20), "FirstName": "Élodie", "LastName": "Exemple", "Email": "person-000002@example.com", "Company": "Synthetic Retail Household", "Status": "Closed - Converted", "IsConverted": true, "ConvertedContactId": sfid("003", 20), "ConvertedAccountId": sfid("001", 1), "ConvertedOpportunityId": nil, "ConvertedDate": "2026-01-01", "OwnerId": sfid("005", 1), "IsDeleted": false}
	if e := write("sources/salesforce/converted-lead.json", lead); e != nil {
		return e
	}
	// Latin-1 historical export is intentionally separate from UTF-8 JSON. Never dial or mail fixtures.
	latin1 := []byte("KUNDENNR;NACHNAME;ORT;LAND;DATUM\r\nERP-OLD-001;M\xfcller;Wien;AUT;31.12.11\r\nERP-OLD-002;M\xfcller;;UK;12/31/11\r\n")
	if e := os.WriteFile(filepath.Join(out, "sources/erp/windows-1252.csv"), latin1, 0600); e != nil {
		return e
	}
	return write("sources/emarsys/field-mapping.json", object{"synthetic_account_configuration": true, "endpoint_family": "PUT /api/v2/contact", "fields": object{"1": "first name", "2": "last name", "3": "email", "9001": "fictional indexed retail external key", "9002": "fictional email-marketing permission string (not universal consent)", "9003": "fictional last-import timestamp"}})
}
func ReplayShuffled(path string, w interface{ Write([]byte) (int, error) }, seed int64) error {
	var events []Event
	if err := readLines(path, func(e Event, _ int) error { events = append(events, e); return nil }); err != nil {
		return err
	}
	sort.Slice(events, func(i, j int) bool {
		return idFor(fmt.Sprintf("%d/%s", seed, events[i].DeliveryID)) < idFor(fmt.Sprintf("%d/%s", seed, events[j].DeliveryID))
	})
	enc := json.NewEncoder(w)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	return nil
}
