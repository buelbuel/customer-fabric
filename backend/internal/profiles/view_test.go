package profiles

import (
	"encoding/json"
	"testing"
	"time"
)

func linkedRecord(id, source string, attrs map[string]string) LinkedRecord {
	r := LinkedRecord{AssociationID: id, Source: source, RecordProfileID: "profile-" + id, Version: 2, SourceOwned: true, Identifiers: 1,
		Attributes: map[string]json.RawMessage{}, Evidence: map[string]AttributeEvidence{}, UpdatedAt: time.Unix(10, 0)}
	for path, value := range attrs {
		r.Attributes[path] = json.RawMessage(value)
		r.Evidence[path] = AttributeEvidence{Owner: source, MutationID: "mutation-" + id, At: time.Unix(5, 0)}
	}
	return r
}

func TestBuildCustomerViewDisclosesEveryVisibleValueWithoutChoosing(t *testing.T) {
	view := BuildCustomerView("anchor", []LinkedRecord{
		linkedRecord("b", "crm", map[string]string{"tier": `"gold"`, "plan": `"x"`}),
		linkedRecord("a", "erp", map[string]string{"tier": `"silver"`, "plan": `"x"`}),
	}, map[string][]string{"crm": {"tier", "plan"}, "erp": {"tier", "plan"}})
	if len(view.Attributes) != 2 || view.Attributes[0].Path != "plan" || view.Attributes[0].Conflict {
		t.Fatalf("agreeing values must not conflict: %+v", view.Attributes)
	}
	tier := view.Attributes[1]
	if tier.Path != "tier" || !tier.Conflict || len(tier.Values) != 2 || tier.Values[0].Source != "crm" || tier.Values[1].Source != "erp" {
		t.Fatalf("both values must stay visible in source order: %+v", tier)
	}
	if tier.Values[0].MutationID != "mutation-b" || tier.Values[0].RecordProfileID != "profile-b" {
		t.Fatalf("a value lost its provenance: %+v", tier.Values[0])
	}
}

func TestBuildCustomerViewWithholdsWhatItCannotAttribute(t *testing.T) {
	foreign := linkedRecord("a", "crm", map[string]string{"owned": `1`, "foreign": `2`, "unproven": `3`, "hidden": `4`})
	foreign.Evidence["foreign"] = AttributeEvidence{Owner: "erp", MutationID: "m", At: time.Unix(1, 0)}
	delete(foreign.Evidence, "unproven")
	view := BuildCustomerView("anchor", []LinkedRecord{foreign}, map[string][]string{"crm": {"owned", "foreign", "unproven"}})
	if len(view.Attributes) != 1 || view.Attributes[0].Path != "owned" {
		t.Fatalf("only the attributed, visible value may appear: %+v", view.Attributes)
	}
	if view.Records[0].WithheldAttributes != 3 {
		t.Fatalf("withheld count = %d", view.Records[0].WithheldAttributes)
	}
}

func TestBuildCustomerViewNoPolicyDisclosesNothing(t *testing.T) {
	view := BuildCustomerView("anchor", []LinkedRecord{linkedRecord("a", "crm", map[string]string{"tier": `"gold"`})}, nil)
	if len(view.Attributes) != 0 || view.Records[0].WithheldAttributes != 1 {
		t.Fatalf("an absent policy must deny: %+v", view)
	}
}

func TestBuildCustomerViewAmbiguousRecordsDiscloseOnlyStatus(t *testing.T) {
	cases := map[RecordStatus]func(*LinkedRecord){
		RecordRestricted:          func(r *LinkedRecord) { r.Restricted = true },
		RecordNotSourceOwned:      func(r *LinkedRecord) { r.SourceOwned = false },
		RecordMultipleIdentifiers: func(r *LinkedRecord) { r.Identifiers = 2 },
	}
	for status, change := range cases {
		r := linkedRecord("a", "crm", map[string]string{"tier": `"gold"`})
		change(&r)
		view := BuildCustomerView("anchor", []LinkedRecord{r}, map[string][]string{"crm": {"tier"}})
		got := view.Records[0]
		if got.Status != status || len(view.Attributes) != 0 || got.RecordProfileID != "" || got.ProfileVersion != 0 || got.UpdatedAt != nil {
			t.Fatalf("%s disclosed record detail: %+v", status, view)
		}
	}
}

func TestBuildCustomerViewEmptyListsEncodeAsArrays(t *testing.T) {
	raw, err := json.Marshal(BuildCustomerView("anchor", nil, nil))
	if err != nil || string(raw) != `{"anchor_profile_id":"anchor","logic":"`+ViewLogic+`","attributes":[],"records":[]}` {
		t.Fatalf("unexpected empty view %s %v", raw, err)
	}
}
