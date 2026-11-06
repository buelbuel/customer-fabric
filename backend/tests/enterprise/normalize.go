package enterprise

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

// NormalizeSnapshot is a deliberately narrow fixture adapter, not a vendor connector.
// It never reads ground truth or decides whether two records identify the same person.
// The caller chooses an explicit, authorized profile target separately.
func NormalizeSnapshot(source string, data []byte) (profiles.Mutation, error) {
	var root object
	if err := json.Unmarshal(data, &root); err != nil {
		return profiles.Mutation{}, err
	}
	m := profiles.Mutation{Set: map[string]json.RawMessage{}}
	var id, name, email any
	switch source {
	case "salesforce":
		if root["IsDeleted"] == true {
			return m, fmt.Errorf("deleted record: quarantine; M0 has no erasure")
		}
		id = root["Id"]
		name = root["LastName"]
		email = root["Email"]
	case "braze":
		users, ok := root["users"].([]any)
		if !ok || len(users) != 1 {
			return m, fmt.Errorf("expected one exported user")
		}
		u, ok := users[0].(map[string]any)
		if !ok {
			return m, fmt.Errorf("invalid user")
		}
		id = u["external_id"]
		name = u["last_name"]
		email = u["email"]
	case "shopify":
		data, ok := root["data"].(map[string]any)
		if !ok {
			return m, fmt.Errorf("invalid GraphQL envelope")
		}
		customers, ok := data["customers"].(map[string]any)
		if !ok {
			return m, fmt.Errorf("missing customers")
		}
		nodes, ok := customers["nodes"].([]any)
		if !ok || len(nodes) != 1 {
			return m, fmt.Errorf("expected one customer")
		}
		u, ok := nodes[0].(map[string]any)
		if !ok {
			return m, fmt.Errorf("invalid customer")
		}
		id = u["id"]
		name = u["lastName"]
		if address, ok := u["defaultEmailAddress"].(map[string]any); ok {
			email = address["emailAddress"]
		}
	case "emarsys":
		if root["key_id"] != "9001" {
			return m, fmt.Errorf("unrecognized synthetic account field mapping")
		}
		contacts, ok := root["contacts"].([]any)
		if !ok || len(contacts) != 1 {
			return m, fmt.Errorf("expected one contact")
		}
		u, ok := contacts[0].(map[string]any)
		if !ok {
			return m, fmt.Errorf("invalid contact")
		}
		id = u["9001"]
		name = u["2"]
		email = u["3"]
	case "erp":
		id = root["KUNDENNR"]
		name = root["NACHNAME"]
		email = root["EMAIL"]
	case "loyalty":
		id = root["membership_id"]
		email = root["email"]
	case "identity":
		return m, fmt.Errorf("identity enrollment is future scope; no Kratos connector")
	default:
		return m, fmt.Errorf("unknown source")
	}
	sid, ok := id.(string)
	if !ok || strings.TrimSpace(sid) == "" {
		return m, fmt.Errorf("missing explicit source identifier")
	}
	for field, value := range map[string]any{"display_name": name, "email": email} {
		if value == nil || value == "" {
			continue
		}
		s, ok := value.(string)
		if !ok {
			return m, fmt.Errorf("%s must be a string", field)
		}
		if field == "email" && !strings.HasSuffix(s, "@example.com") {
			return m, fmt.Errorf("fixture email must use reserved example.com")
		}
		m.Set[field], _ = json.Marshal(s)
	}
	m.Identifiers = []profiles.IdentifierInput{{Namespace: source, Value: sid}}
	return m, nil
}

// NormalizeContactCDC consumes CometD JSON only. No Avro bitmap interpretation.
func NormalizeContactCDC(data []byte) (string, profiles.Mutation, error) {
	var envelope struct {
		Data struct {
			Payload map[string]json.RawMessage `json:"payload"`
		} `json:"data"`
	}
	var m profiles.Mutation
	m.Set = map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &envelope); err != nil {
		return "", m, err
	}
	payload := envelope.Data.Payload
	var h struct {
		Entity string   `json:"entityName"`
		Type   string   `json:"changeType"`
		IDs    []string `json:"recordIds"`
	}
	if err := json.Unmarshal(payload["ChangeEventHeader"], &h); err != nil {
		return "", m, err
	}
	if h.Entity != "Contact" || h.Type != "UPDATE" || len(h.IDs) != 1 {
		return "", m, fmt.Errorf("only single Contact UPDATE is supported")
	}
	for _, field := range []struct{ vendor, target string }{{"Email", "email"}, {"LastName", "display_name"}, {"Phone", "phone"}} {
		value, present := payload[field.vendor]
		if !present {
			continue
		}
		if string(value) == "null" {
			m.Remove = append(m.Remove, field.target)
			continue
		}
		var s string
		if err := json.Unmarshal(value, &s); err != nil {
			return "", m, fmt.Errorf("invalid %s", field.vendor)
		}
		m.Set[field.target] = value
	}
	if len(m.Set)+len(m.Remove) == 0 {
		return "", m, fmt.Errorf("no supported changed fields")
	}
	return h.IDs[0], m, nil
}

// Score is test-only. A false positive is never hidden inside an aggregate accuracy score.
type Score struct {
	FalsePositiveMerges int `json:"false_positive_merges"`
	MissedMatches       int `json:"missed_matches"`
	UnknownPredictions  int `json:"unknown_predictions"`
}

func Evaluate(groundTruth []Link, predicted [][2]string) Score {
	key := func(a, b string) string {
		if a > b {
			a, b = b, a
		}
		return a + "\x00" + b
	}
	actual := map[string]string{}
	for _, l := range groundTruth {
		actual[key(l.Left, l.Right)] = l.Class
	}
	seen := map[string]bool{}
	var s Score
	for _, pair := range predicted {
		k := key(pair[0], pair[1])
		if seen[k] {
			continue
		}
		seen[k] = true
		switch actual[k] {
		case "verified", "duplicate_source", "ambiguous":
		case "different", "unsafe":
			s.FalsePositiveMerges++
		default:
			s.UnknownPredictions++
		}
	}
	for k, class := range actual {
		if (class == "verified" || class == "duplicate_source") && !seen[k] {
			s.MissedMatches++
		}
	}
	return s
}

func normalizationRejected(source string, payload object) bool {
	if source == "identity" {
		return false
	}
	data, _ := json.Marshal(payload)
	_, err := NormalizeSnapshot(source, data)
	return err != nil
}

// RequestJSON omits absent operations instead of encoding null (which M0 correctly rejects).
func RequestJSON(m profiles.Mutation) ([]byte, error) {
	body := object{}
	if len(m.Set) > 0 {
		body["set"] = m.Set
	}
	if len(m.Remove) > 0 {
		body["remove"] = m.Remove
	}
	if len(m.Identifiers) > 0 {
		body["identifiers"] = m.Identifiers
	}
	return json.Marshal(body)
}
