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

// governed defines each path as a string under revision 1 and lets the operator read it from sources.
func governed(sources []string, paths ...string) Governance {
	g := Governance{Definitions: map[string]AttributeDefinition{}, Policies: map[string]AccessPolicy{}}
	for _, path := range paths {
		g.Definitions[path] = AttributeDefinition{Path: path, Revision: 1, Type: TypeString}
		g.Policies[path] = AccessPolicy{Path: path, Revision: 1, Readers: []Reader{ReaderOperator}, Sources: sources}
	}
	return g
}

func TestBuildCustomerViewDisclosesEveryVisibleValueWithoutChoosing(t *testing.T) {
	view := BuildCustomerView("anchor", ReaderOperator, []LinkedRecord{
		linkedRecord("b", "crm", map[string]string{"tier": `"gold"`, "plan": `"x"`}),
		linkedRecord("a", "erp", map[string]string{"tier": `"silver"`, "plan": `"x"`}),
	}, governed([]string{"crm", "erp"}, "tier", "plan"))
	if len(view.Attributes) != 2 || view.Attributes[0].Path != "plan" || view.Attributes[0].Conflict {
		t.Fatalf("agreeing values must not conflict: %+v", view.Attributes)
	}
	tier := view.Attributes[1]
	if tier.Path != "tier" || !tier.Conflict || len(tier.Values) != 2 || tier.Values[0].Source != "crm" || tier.Values[1].Source != "erp" {
		t.Fatalf("both values must stay visible in source order: %+v", tier)
	}
	if tier.Type != TypeString || tier.DefinitionRevision != 1 || tier.PolicyRevision != 1 {
		t.Fatalf("an attribute lost its governance: %+v", tier)
	}
	if tier.Values[0].MutationID != "mutation-b" || tier.Values[0].RecordProfileID != "profile-b" {
		t.Fatalf("a value lost its provenance: %+v", tier.Values[0])
	}
}

func TestBuildCustomerViewWithholdsWhatItCannotAttribute(t *testing.T) {
	foreign := linkedRecord("a", "crm", map[string]string{"owned": `1`, "foreign": `2`, "unproven": `3`, "hidden": `4`})
	foreign.Evidence["foreign"] = AttributeEvidence{Owner: "erp", MutationID: "m", At: time.Unix(1, 0)}
	delete(foreign.Evidence, "unproven")
	view := BuildCustomerView("anchor", ReaderOperator, []LinkedRecord{foreign}, governed([]string{"crm"}, "owned", "foreign", "unproven"))
	if len(view.Attributes) != 1 || view.Attributes[0].Path != "owned" {
		t.Fatalf("only the attributed, visible value may appear: %+v", view.Attributes)
	}
	if view.Records[0].WithheldAttributes != 3 {
		t.Fatalf("withheld count = %d", view.Records[0].WithheldAttributes)
	}
}

func TestBuildCustomerViewNoPolicyDisclosesNothing(t *testing.T) {
	view := BuildCustomerView("anchor", ReaderOperator, []LinkedRecord{linkedRecord("a", "crm", map[string]string{"tier": `"gold"`})}, Governance{})
	if len(view.Attributes) != 0 || view.Records[0].WithheldAttributes != 1 {
		t.Fatalf("an absent policy must deny: %+v", view)
	}
}

// Every missing piece of governance denies on its own.
func TestBuildCustomerViewDeniesByDefault(t *testing.T) {
	record := []LinkedRecord{linkedRecord("a", "crm", map[string]string{"tier": `"gold"`})}
	undefined := governed([]string{"crm"}, "tier")
	delete(undefined.Definitions, "tier")
	unpolicied := governed([]string{"crm"}, "tier")
	delete(unpolicied.Policies, "tier")
	otherSource := governed([]string{"erp"}, "tier")
	withdrawn := governed([]string{"crm"}, "tier")
	withdrawn.Policies["tier"] = AccessPolicy{Path: "tier", Revision: 2, Readers: []Reader{}, Sources: []string{"crm"}}
	for name, g := range map[string]Governance{"undefined": undefined, "no policy": unpolicied, "other source": otherSource, "withdrawn": withdrawn} {
		if view := BuildCustomerView("anchor", ReaderOperator, record, g); len(view.Attributes) != 0 || view.Records[0].WithheldAttributes != 1 {
			t.Errorf("%s disclosed: %+v", name, view)
		}
	}
	segmentOnly := governed([]string{"crm"}, "tier")
	segmentOnly.Policies["tier"] = AccessPolicy{Path: "tier", Revision: 1, Readers: []Reader{ReaderSegmentation}, Sources: []string{"crm"}}
	if view := BuildCustomerView("anchor", ReaderOperator, record, segmentOnly); len(view.Attributes) != 0 {
		t.Fatal("a segmentation-only attribute was shown to the operator")
	}
	if view := BuildCustomerView("anchor", ReaderSegmentation, record, segmentOnly); len(view.Attributes) != 1 || view.Reader != ReaderSegmentation {
		t.Fatalf("segmentation was denied its attribute: %+v", view)
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
		view := BuildCustomerView("anchor", ReaderOperator, []LinkedRecord{r}, governed([]string{"crm"}, "tier"))
		got := view.Records[0]
		if got.Status != status || len(view.Attributes) != 0 || got.RecordProfileID != "" || got.ProfileVersion != 0 || got.UpdatedAt != nil {
			t.Fatalf("%s disclosed record detail: %+v", status, view)
		}
	}
}

func TestBuildCustomerViewEmptyListsEncodeAsArrays(t *testing.T) {
	raw, err := json.Marshal(BuildCustomerView("anchor", ReaderOperator, nil, Governance{}))
	if err != nil || string(raw) != `{"anchor_profile_id":"anchor","logic":"`+ViewLogic+`","reader":"operator","attributes":[],"records":[]}` {
		t.Fatalf("unexpected empty view %s %v", raw, err)
	}
}
