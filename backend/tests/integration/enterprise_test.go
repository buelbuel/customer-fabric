package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/customer-fabric/customer-fabric/backend/tests/enterprise"
)

// Tests the fixture adapter independently of any future connector/matching engine. Every request goes
// through a real HTTP server and PostgreSQL in an isolated test schema. Shopify's record unites with
// Salesforce's only through a trusted identity key, never by naming the profile (ADR 0018).
func TestEnterpriseFixtureScenarios(t *testing.T) {
	s := setup(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO sources(name) VALUES('salesforce'),('shopify')`); err != nil {
		t.Fatal(err)
	}
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", KratosPublicURL: "http://customer-kratos.invalid:4433", OperatorKratosPublicURL: newOperatorIssuer(t), Tokens: map[string]string{"salesforce": tokenA, "shopify": tokenB}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(h)
	defer server.Close()
	request := func(method, path string, body []byte, key, token, version string, status int) []byte {
		t.Helper()
		r, err := http.NewRequest(method, server.URL+path, bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		r.Header.Set("Authorization", "Bearer "+token)
		r.Header.Set("Content-Type", "application/json")
		if key != "" {
			r.Header.Set("Idempotency-Key", key)
		}
		if version != "" {
			r.Header.Set("If-Match", version)
		}
		response, err := server.Client().Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		data, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != status {
			t.Fatalf("%s %s: want %d got %d %s", method, path, status, response.StatusCode, data)
		}
		return data
	}
	sf, err := enterprise.NormalizeSnapshot("salesforce", enterprise.Sample("salesforce"))
	if err != nil {
		t.Fatal(err)
	}
	key := profiles.IdentifierInput{Namespace: "customer_number", Value: "C-ENT-1"}
	sf.Identifiers = append(sf.Identifiers, key)
	create, err := enterprise.RequestJSON(sf)
	if err != nil {
		t.Fatal(err)
	}
	created := request("POST", "/v1/profiles", create, "enterprise-create-0001", tokenA, "", 201)
	var receipt profiles.Receipt
	if err = json.Unmarshal(created, &receipt); err != nil {
		t.Fatal(err)
	}
	path := "/v1/profiles/" + receipt.ID
	// Shopify keeps its own record, with an overlapping display name. Naming Salesforce's profile gives
	// it nothing; the trusted key both records present unites them.
	shop, err := enterprise.NormalizeSnapshot("shopify", enterprise.Sample("shopify"))
	if err != nil {
		t.Fatal(err)
	}
	shop.Identifiers = append(shop.Identifiers, key)
	if shop.Set == nil {
		shop.Set = map[string]json.RawMessage{}
	}
	shop.Set["display_name"] = json.RawMessage(`"Conflicting commerce value"`)
	shopBody, err := enterprise.RequestJSON(shop)
	if err != nil {
		t.Fatal(err)
	}
	request("PATCH", path, shopBody, "enterprise-link-00000", tokenB, `"1"`, 403)
	trustKey(t, h, viewOperator, "customer_number", "salesforce", "shopify")
	first := request("POST", "/v1/profiles", shopBody, "enterprise-shop-00001", tokenB, "", 201)
	again := request("POST", "/v1/profiles", shopBody, "enterprise-shop-00001", tokenB, "", 201)
	var shopReceipt, againReceipt profiles.Receipt
	_ = json.Unmarshal(first, &shopReceipt)
	_ = json.Unmarshal(again, &againReceipt)
	if shopReceipt != againReceipt || shopReceipt.ID == receipt.ID {
		t.Fatal("shopify record receipt", shopReceipt, againReceipt)
	}
	if _, err = (platform.Store{Pool: s.pool}).ReconcileIdentities(context.Background()); err != nil {
		t.Fatal(err)
	}
	var customer profiles.Customer
	if err = json.Unmarshal(request("GET", "/v1/operator/profiles/"+shopReceipt.ID, nil, "", viewOperator, "", 200), &customer); err != nil {
		t.Fatal(err)
	}
	shopify := 0
	for _, id := range customer.Identifiers {
		if id.Source == "shopify" {
			shopify++
		}
	}
	if customer.ID != receipt.ID || len(customer.Records) != 2 || shopify != 2 || customer.ResolvedFrom["display_name"] != receipt.ID ||
		string(customer.Records[1].Attributes["display_name"]) != `"Conflicting commerce value"` {
		t.Fatal(customer)
	}
	if s.count("profiles") != 2 || s.count("outbox") != 3 {
		t.Fatal("unification changed records or announced the wrong profiles")
	}
	request("PATCH", path, []byte(`{"set":{"display_name":"Current synthetic name"}}`), "enterprise-update-0001", tokenA, `"1"`, 200)
	// Duplicate delivery after subsequent mutations retains the original small receipt.
	replay := request("POST", "/v1/profiles", create, "enterprise-create-0001", tokenA, "", 201)
	var replayReceipt profiles.Receipt
	if err = json.Unmarshal(replay, &replayReceipt); err != nil || replayReceipt != receipt {
		t.Fatal("receipt changed", err)
	}
	request("PATCH", path, []byte(`{"set":{"display_name":"Historical backfill"}}`), "enterprise-backfill-001", tokenA, `"1"`, 412)
	request("POST", "/v1/profiles", []byte(`{"set":{"display_name":[]}}`), "enterprise-partial-bad", tokenA, "", 422)
	if s.count("profiles") != 2 || s.count("outbox") != 4 {
		t.Fatal("failed batch item changed state")
	}
	// A separate source record with the same email does not cause an automatic merge.
	sf.Identifiers = []profiles.IdentifierInput{{Namespace: "salesforce", Value: "003000000000999"}}
	second, err := enterprise.RequestJSON(sf)
	if err != nil {
		t.Fatal(err)
	}
	created = request("POST", "/v1/profiles", second, "enterprise-partial-good", tokenA, "", 201)
	if err = json.Unmarshal(created, &receipt); err != nil {
		t.Fatal(err)
	}
	if _, err = (platform.Store{Pool: s.pool}).ReconcileIdentities(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.count("profiles") != 3 || s.count("profile_aliases") != 1 {
		t.Fatal("partial import/independent profile failed")
	}
}
