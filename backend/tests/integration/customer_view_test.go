package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

var viewOperator = strings.Repeat("o", 64)

func (s linkingSuite) viewRecord(token, value, set string, extra ...string) string {
	s.t.Helper()
	identifiers := []string{fmt.Sprintf(`{"namespace":"crm","value":%q}`, value)}
	for _, alias := range extra {
		identifiers = append(identifiers, fmt.Sprintf(`{"namespace":"crm","value":%q}`, alias))
	}
	res := s.request("POST", "/v1/profiles", fmt.Sprintf(`{"set":%s,"identifiers":[%s]}`, set, strings.Join(identifiers, ",")), platform.UUID(), token, "")
	s.expect(res, 201)
	var receipt profiles.Receipt
	if err := json.Unmarshal(res.Body.Bytes(), &receipt); err != nil {
		s.t.Fatal(err)
	}
	return receipt.ID
}

// viewLink issues and redeems a grant, returning the association and grant IDs.
func (s linkingSuite) viewLink(issuer, customer, value string) (string, string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"namespace":"crm","record":%q,"customer_subject":%q,"proof_method":"synthetic_challenge","proof_reference":%q}`,
		value, s.customerIDs[customer], platform.UUID())
	res := s.request("POST", linkGrants, body, "", issuer, "")
	s.expect(res, 201)
	var grant testLinkGrant
	if err := json.Unmarshal(res.Body.Bytes(), &grant); err != nil {
		s.t.Fatal(err)
	}
	res = s.customer("POST", associations, grant.payload(), customer)
	s.expect(res, 201)
	var association struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &association); err != nil {
		s.t.Fatal(err)
	}
	return association.ID, grant.ID
}

func (s linkingSuite) anchor(customer string) string {
	s.t.Helper()
	res := s.customer("GET", "/v1/customer/profile", "", customer)
	s.expect(res, 200)
	var anchor struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &anchor); err != nil {
		s.t.Fatal(err)
	}
	return anchor.ID
}

// govern defines an attribute and grants it an access policy through the operator API.
func (s linkingSuite) govern(path, definition, policy string) {
	s.t.Helper()
	s.expect(s.request("POST", "/v1/operator/attributes/"+path, definition, "", viewOperator, ""), 201)
	s.expect(s.request("POST", "/v1/operator/attributes/"+path+"/access", policy, "", viewOperator, ""), 201)
}

// setupView governs the attributes these tests read: console's display_name, tier, plan and status,
// and importer's tier. Anything else stays undefined and so withheld.
func setupView(t *testing.T) linkingSuite {
	s := setupLinking(t)
	s.govern("tier", `{"type":"string"}`, `{"readers":["operator"],"sources":["console","importer"]}`)
	for _, path := range []string{"display_name", "plan", "status"} {
		s.govern(path, `{"type":"string"}`, `{"readers":["operator"],"sources":["console"]}`)
	}
	return s
}

func viewPath(anchor string) string { return "/v1/operator/customers/" + anchor + "/unified-view" }

func (s linkingSuite) rawView(customer string) *httptest.ResponseRecorder {
	s.t.Helper()
	res := s.request("GET", viewPath(s.anchor(customer)), "", "", viewOperator, "")
	s.expect(res, 200)
	return res
}

func (s linkingSuite) view(customer string) profiles.CustomerView {
	s.t.Helper()
	var view profiles.CustomerView
	if err := json.Unmarshal(s.rawView(customer).Body.Bytes(), &view); err != nil {
		s.t.Fatal(err)
	}
	return view
}

func viewPaths(view profiles.CustomerView) string {
	paths := []string{}
	for _, attribute := range view.Attributes {
		paths = append(paths, attribute.Path)
	}
	return strings.Join(paths, ",")
}

func TestCustomerViewDisclosesVerifiedRecordsWithoutMerging(t *testing.T) {
	s := setupView(t)
	crm := s.viewRecord(tokenA, "view-crm", `{"display_name":"Synthetic Ada","tier":"gold","risk_score":7}`)
	erp := s.viewRecord(tokenB, "view-erp", `{"tier":"silver"}`)
	crmLink, _ := s.viewLink(linkIssuerA, linkCustomerA, "view-crm")
	erpLink, _ := s.viewLink(linkIssuerB, linkCustomerA, "view-erp")
	before := s.customer("GET", "/v1/customer/profile", "", linkCustomerA).Body.String()
	writes := s.count("mutations") + s.count("provenance") + s.count("outbox") + s.count("attribute_contributions")

	res := s.rawView(linkCustomerA)
	if strings.Contains(res.Body.String(), "risk_score") {
		t.Fatal("an attribute outside the policy was disclosed")
	}
	var view profiles.CustomerView
	if err := json.Unmarshal(res.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.AnchorProfileID != s.anchor(linkCustomerA) || view.Logic != profiles.ViewLogic || viewPaths(view) != "display_name,tier" {
		t.Fatalf("unexpected view %+v", view)
	}
	if view.Attributes[0].Conflict || len(view.Attributes[0].Values) != 1 {
		t.Fatalf("a single value cannot conflict: %+v", view.Attributes[0])
	}
	tier := view.Attributes[1]
	if !tier.Conflict || len(tier.Values) != 2 {
		t.Fatalf("both values must be listed as a conflict: %+v", tier)
	}
	want := map[string]struct{ value, association, profile string }{
		"console":  {`"gold"`, crmLink, crm},
		"importer": {`"silver"`, erpLink, erp},
	}
	for _, value := range tier.Values {
		expected := want[value.Source]
		if string(value.Value) != expected.value || value.AssociationID != expected.association || value.RecordProfileID != expected.profile {
			t.Fatalf("value lost its source record: %+v", value)
		}
		var mutation string
		if err := s.pool.QueryRow(context.Background(), `SELECT mutation_id FROM provenance WHERE profile_id=$1 AND path='tier' AND outcome='accepted'`, value.RecordProfileID).Scan(&mutation); err != nil {
			t.Fatal(err)
		}
		if mutation != value.MutationID || value.WrittenAt.IsZero() {
			t.Fatalf("value provenance does not match the write: %+v", value)
		}
	}
	withheld := 0
	for _, record := range view.Records {
		if record.Status != profiles.RecordIncluded || record.ProfileVersion != 1 || record.UpdatedAt == nil {
			t.Fatalf("unexpected record %+v", record)
		}
		withheld += record.WithheldAttributes
	}
	if len(view.Records) != 2 || withheld != 1 {
		t.Fatalf("records %+v withheld %d", view.Records, withheld)
	}

	if s.customer("GET", "/v1/customer/profile", "", linkCustomerA).Body.String() != before {
		t.Fatal("the view changed the customer anchor")
	}
	if got := s.count("mutations") + s.count("provenance") + s.count("outbox") + s.count("attribute_contributions"); got != writes {
		t.Fatal("reading the view wrote profile state")
	}
	var owners int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM attribute_contributions WHERE profile_id=$1`, view.AnchorProfileID).Scan(&owners); err != nil || owners != 0 {
		t.Fatalf("the anchor gained attribute ownership: %d %v", owners, err)
	}

	other := s.view(linkCustomerB)
	if len(other.Records) != 0 || len(other.Attributes) != 0 {
		t.Fatalf("another customer's view contains these records: %+v", other)
	}
}

func TestCustomerViewAccessBoundaries(t *testing.T) {
	s := setupView(t)
	record := s.viewRecord(tokenA, "view-boundary", `{"tier":"gold"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "view-boundary")
	path := viewPath(s.anchor(linkCustomerA))
	for name, token := range map[string]string{"source": tokenA, "other source": tokenB, "issuer": linkIssuerA, "unknown": "invalid"} {
		if res := s.request("GET", path, "", "", token, ""); res.Code != 401 {
			t.Fatalf("%s credential: %d", name, res.Code)
		}
	}
	if res := s.customer("GET", path, "", linkCustomerA); res.Code != 401 {
		t.Fatalf("customer session: %d", res.Code)
	}
	s.expect(s.request("GET", viewPath(record), "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", viewPath(platform.UUID()), "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", "/v1/operator/customers/not-a-uuid/unified-view", "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", path+"/extra", "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", path+"?source=console", "", "", viewOperator, ""), 400)
	s.expect(s.request("POST", path, "", "", viewOperator, ""), 405)
}

func TestCustomerViewDropsEndedAssociations(t *testing.T) {
	s := setupView(t)
	s.viewRecord(tokenA, "view-unlinked", `{"tier":"gold"}`)
	s.viewRecord(tokenA, "view-revoked", `{"plan":"basic"}`)
	deleted := s.viewRecord(tokenA, "view-deleted", `{"status":"active"}`)
	unlinked, _ := s.viewLink(linkIssuerA, linkCustomerA, "view-unlinked")
	_, revoked := s.viewLink(linkIssuerA, linkCustomerA, "view-revoked")
	s.viewLink(linkIssuerA, linkCustomerA, "view-deleted")
	if got := viewPaths(s.view(linkCustomerA)); got != "plan,status,tier" {
		t.Fatalf("linked attributes: %s", got)
	}
	s.expect(s.customer("DELETE", associations+"/"+unlinked, "", linkCustomerA), 204)
	if got := viewPaths(s.view(linkCustomerA)); got != "plan,status" {
		t.Fatalf("customer unlink stayed visible: %s", got)
	}
	s.expect(s.request("DELETE", linkGrants+"/"+revoked, "", "", linkIssuerA, ""), 204)
	if got := viewPaths(s.view(linkCustomerA)); got != "status" {
		t.Fatalf("issuer revocation stayed visible: %s", got)
	}
	s.exec("DELETE FROM external_identifiers WHERE source='console' AND value='view-deleted'")
	view := s.view(linkCustomerA)
	if len(view.Records) != 0 || len(view.Attributes) != 0 {
		t.Fatalf("a deleted identifier stayed visible: %+v", view)
	}
	s.exec("INSERT INTO external_identifiers(source, namespace, value, profile_id) VALUES('console','crm','view-deleted',$1)", deleted)
	if view = s.view(linkCustomerA); len(view.Records) != 0 {
		t.Fatalf("a re-created identifier revived its old association: %+v", view)
	}
}

func TestCustomerViewAmbiguousRecordsDiscloseNoValues(t *testing.T) {
	s := setupView(t)
	s.viewRecord(tokenA, "view-shared-a", `{"tier":"gold"}`, "view-shared-b")
	s.viewLink(linkIssuerA, linkCustomerA, "view-shared-a")
	s.viewLink(linkIssuerA, linkCustomerB, "view-shared-b")
	for _, customer := range []string{linkCustomerA, linkCustomerB} {
		res := s.rawView(customer)
		var view profiles.CustomerView
		if err := json.Unmarshal(res.Body.Bytes(), &view); err != nil {
			t.Fatal(err)
		}
		if len(view.Records) != 1 || view.Records[0].Status != profiles.RecordMultipleIdentifiers || len(view.Attributes) != 0 ||
			view.Records[0].RecordProfileID != "" || strings.Contains(res.Body.String(), "gold") {
			t.Fatalf("a record shared by two customers disclosed data: %s", res.Body.String())
		}
	}
}

func TestCustomerViewFencesRecordsItCannotTrust(t *testing.T) {
	s := setupView(t)
	foreign := s.viewRecord(tokenA, "view-creator", `{"tier":"gold"}`)
	s.viewRecord(tokenA, "view-erasing", `{"plan":"basic"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "view-creator")
	s.viewLink(linkIssuerA, linkCustomerA, "view-erasing")
	status := func(view profiles.CustomerView) map[profiles.RecordStatus]int {
		counts := map[profiles.RecordStatus]int{}
		for _, record := range view.Records {
			counts[record.Status]++
		}
		return counts
	}

	s.exec("UPDATE profiles SET creator_source='importer' WHERE id=$1", foreign)
	view := s.view(linkCustomerA)
	if got := status(view); got[profiles.RecordNotSourceOwned] != 1 || viewPaths(view) != "plan" {
		t.Fatalf("a record its source does not own was disclosed: %+v", view)
	}

	s.exec(`INSERT INTO consent_subjects(id, kind, anchor_record_epoch, lifecycle_state)
		SELECT $1, 'source_record', record_epoch, 'erasure_pending' FROM external_identifiers WHERE value='view-erasing'`, platform.UUID())
	view = s.view(linkCustomerA)
	if got := status(view); got[profiles.RecordRestricted] != 1 || len(view.Attributes) != 0 {
		t.Fatalf("a record pending erasure was disclosed: %+v", view)
	}

	s.exec("UPDATE consent_subjects SET lifecycle_state='active'")
	s.exec("UPDATE profiles SET creator_source='console' WHERE id=$1", foreign)
	if view = s.view(linkCustomerA); viewPaths(view) != "plan,tier" {
		t.Fatalf("restoring trust must restore values: %+v", view)
	}
	s.exec("UPDATE profiles SET restricted=true WHERE id=$1", foreign)
	view = s.view(linkCustomerA)
	if got := status(view); got[profiles.RecordRestricted] != 1 || viewPaths(view) != "plan" {
		t.Fatalf("a restricted profile was disclosed: %+v", view)
	}
	s.exec(`INSERT INTO consent_subjects(id, kind, anchor_profile_id, lifecycle_state) VALUES($1, 'customer', $2, 'erased')`,
		platform.UUID(), view.AnchorProfileID)
	s.expect(s.request("GET", viewPath(view.AnchorProfileID), "", "", viewOperator, ""), 409)
}

func TestCustomerViewWithholdsUnattributableAttributes(t *testing.T) {
	s := setupView(t)
	record := s.viewRecord(tokenA, "view-attributes", `{"display_name":"Synthetic","tier":"gold","plan":"basic","status":"active"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "view-attributes")

	s.exec("UPDATE attribute_contributions SET source='importer' WHERE profile_id=$1 AND path='tier'", record)
	s.exec("DELETE FROM provenance WHERE profile_id=$1 AND path='plan'", record)
	s.expect(s.request("PATCH", "/v1/profiles/"+record, `{"remove":["status"]}`, platform.UUID(), tokenA, `"1"`), 200)

	res := s.request("GET", viewPath(s.anchor(linkCustomerA)), "", "", viewOperator, "")
	s.expect(res, 200)
	var view profiles.CustomerView
	if err := json.Unmarshal(res.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if viewPaths(view) != "display_name" || view.Records[0].WithheldAttributes != 2 || view.Records[0].ProfileVersion != 2 {
		t.Fatalf("unexpected disclosure %s", res.Body.String())
	}
	for _, leaked := range []string{"gold", "basic", "active"} {
		if strings.Contains(res.Body.String(), `"`+leaked+`"`) {
			t.Fatalf("withheld value %s was disclosed", leaked)
		}
	}
}

func TestCustomerViewRefusesRatherThanTruncates(t *testing.T) {
	s := setupView(t)
	for i := 0; i < 51; i++ {
		value := fmt.Sprintf("view-limit-%d", i)
		s.viewRecord(tokenA, value, `{"tier":"gold"}`)
		s.viewLink(linkIssuerA, linkCustomerA, value)
	}
	s.expect(s.request("GET", viewPath(s.anchor(linkCustomerA)), "", "", viewOperator, ""), 409)
}
