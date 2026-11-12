package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

func TestAccessPoliciesGovernOnlyDefinedAttributes(t *testing.T) {
	s := setupLinking(t)
	access := "/v1/operator/attributes/tier/access"
	policy := `{"readers":["operator"],"sources":["console"]}`
	s.expect(s.request("POST", access, policy, "", tokenA, ""), 401)
	s.expect(s.request("GET", access, "", "", viewOperator, ""), 404)
	s.expect(s.request("POST", access, policy, "", viewOperator, ""), 404)
	s.expect(s.request("POST", access, `{"readers":["customer"],"sources":[]}`, "", viewOperator, ""), 404)
	s.expect(s.request("POST", "/v1/operator/attributes/tier", `{"type":"string"}`, "", viewOperator, ""), 201)

	empty := s.request("GET", access, "", "", viewOperator, "")
	s.expect(empty, 200)
	if strings.TrimSpace(empty.Body.String()) != `{"items":[]}` {
		t.Fatalf("a defined attribute starts without a policy: %s", empty.Body.String())
	}
	for _, invalid := range []string{
		`{"readers":["customer"],"sources":["console"]}`,
		`{"readers":["operator"],"sources":["unknown_source"]}`,
		`{"readers":["operator","operator"],"sources":["console"]}`,
		`{"readers":["operator"]}`,
	} {
		s.expect(s.request("POST", access, invalid, "", viewOperator, ""), 422)
	}
	s.expect(s.request("POST", access, `{"readers":["operator"],"sources":["console"],"values":[]}`, "", viewOperator, ""), 400)
	created := s.request("POST", access, policy, "", viewOperator, "")
	s.expect(created, 201)
	if created.Header().Get("ETag") != `"1"` {
		t.Fatalf("etag %q", created.Header().Get("ETag"))
	}
	s.expect(s.request("POST", access, policy, "", viewOperator, ""), 428)
	s.expect(s.request("POST", access, policy, "", viewOperator, `"2"`), 412)
	s.expect(s.request("POST", access, `{"readers":[],"sources":[]}`, "", viewOperator, `"1"`), 201)
	history := s.request("GET", access, "", "", viewOperator, "")
	s.expect(history, 200)
	if strings.Count(history.Body.String(), `"revision"`) != 2 {
		t.Fatalf("history %s", history.Body.String())
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE attribute_access_policies SET readers='{operator}'`); err == nil {
		t.Fatal("a policy revision was edited in place")
	}
}

// The Epic 2 scenario: two sources disagree, one field is restricted to segmentation, one is
// undefined, and a narrowed policy and a revoked association each take effect on the next read.
func TestGovernedViewDisclosesOnlyWhatPolicyAllows(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["operator","segmentation"],"sources":["console","importer"]}`)
	s.govern("lifetime_value", `{"type":"decimal"}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.govern("display_name", `{"type":"string"}`, `{"readers":["operator"],"sources":["console"]}`)
	s.viewRecord(tokenA, "gov-crm", `{"tier":"gold","lifetime_value":120.5,"display_name":"Synthetic Ada","notes":"synthetic-undefined"}`)
	s.viewRecord(tokenB, "gov-erp", `{"tier":"silver","display_name":"Synthetic Erp"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "gov-crm")
	erpLink, _ := s.viewLink(linkIssuerB, linkCustomerA, "gov-erp")

	res := s.rawView(linkCustomerA)
	body := res.Body.String()
	var view profiles.CustomerView
	if err := json.Unmarshal(res.Body.Bytes(), &view); err != nil {
		t.Fatal(err)
	}
	if view.Reader != profiles.ReaderOperator || viewPaths(view) != "display_name,tier" {
		t.Fatalf("operator view %s", body)
	}
	for _, leaked := range []string{"synthetic-undefined", "notes", "120.5", "lifetime_value", "Synthetic Erp"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("withheld %q was disclosed: %s", leaked, body)
		}
	}
	tier := view.Attributes[1]
	if !tier.Conflict || len(tier.Values) != 2 || tier.Type != profiles.TypeEnum || tier.DefinitionRevision != 1 || tier.PolicyRevision != 1 {
		t.Fatalf("conflicting governed values %+v", tier)
	}
	for _, v := range tier.Values {
		if v.AttributeRevision == nil || *v.AttributeRevision != 1 || v.MutationID == "" {
			t.Fatalf("a value lost its provenance %+v", v)
		}
	}
	withheld := map[string]int{}
	for _, r := range view.Records {
		withheld[r.Source] = r.WithheldAttributes
	}
	if withheld["console"] != 2 || withheld["importer"] != 1 {
		t.Fatalf("withheld %v", withheld)
	}

	// Segmentation reads its own grant, and still never the undefined attribute.
	anchor := s.anchor(linkCustomerA)
	segment, err := platform.Store{Pool: s.pool}.CustomerView(context.Background(), anchor, profiles.ReaderSegmentation)
	if err != nil {
		t.Fatal(err)
	}
	if viewPaths(segment) != "lifetime_value,tier" {
		t.Fatalf("segmentation view %+v", segment)
	}
	if activation, err := (platform.Store{Pool: s.pool}).CustomerView(context.Background(), anchor, profiles.ReaderActivation); err != nil || len(activation.Attributes) != 0 {
		t.Fatalf("activation has no grant: %+v %v", activation, err)
	}

	// Narrowing the policy withdraws importer's tier immediately; ownership and provenance are untouched.
	writes := s.count("attribute_contributions") + s.count("provenance")
	s.expect(s.request("POST", "/v1/operator/attributes/tier/access", `{"readers":["operator","segmentation"],"sources":["console"]}`, "", viewOperator, `"1"`), 201)
	view = s.view(linkCustomerA)
	if tier := view.Attributes[1]; tier.Conflict || len(tier.Values) != 1 || tier.Values[0].Source != "console" || tier.PolicyRevision != 2 {
		t.Fatalf("a narrowed policy still disclosed: %+v", tier)
	}
	if s.count("attribute_contributions")+s.count("provenance") != writes {
		t.Fatal("a policy change touched ownership or provenance")
	}

	// A revoked association removes its record entirely.
	s.expect(s.customer("DELETE", associations+"/"+erpLink, "", linkCustomerA), 204)
	if view = s.view(linkCustomerA); len(view.Records) != 1 || view.Records[0].Source != "console" {
		t.Fatalf("a revoked association stayed visible: %+v", view.Records)
	}
}
