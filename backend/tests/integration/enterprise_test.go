package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/customer-fabric/customer-fabric/backend/tests/enterprise"
)

// Tests the fixture adapter independently of any future connector/matching engine. Every request goes
// through a real HTTP server and PostgreSQL in an isolated test schema. Shopify's record joins
// Salesforce's profile only through a trusted identity key, never by naming the profile (ADR 0018).
func TestEnterpriseFixtureScenarios(t *testing.T) {
	s := setup(t)
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO sources(name) VALUES('salesforce'),('shopify')`); err != nil {
		t.Fatal(err)
	}
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", OperatorToken: viewOperator, Tokens: map[string]string{"salesforce": tokenA, "shopify": tokenB}})
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
	// Explicit linking is an instruction by the test caller, never inferred from matching email.
	shop, err := enterprise.NormalizeSnapshot("shopify", enterprise.Sample("shopify"))
	if err != nil {
		t.Fatal(err)
	}
	shop.Set = nil
	link, err := enterprise.RequestJSON(shop)
	if err != nil {
		t.Fatal(err)
	}
	target, err := enterprise.ExplicitTarget([]string{receipt.ID}, receipt.ID)
	if err != nil || target != receipt.ID {
		t.Fatal(target, err)
	}
	request("PATCH", path, link, "enterprise-link-00000", tokenB, `"1"`, 403)
	version := join(t, h, viewOperator, receipt.ID, 1, tokenA, "salesforce", tokenB, "shopify")
	request("PATCH", path, link, "enterprise-link-00001", tokenB, fmt.Sprintf(`"%d"`, version), 200)
	version++
	rejected := request("PATCH", path, []byte(`{"set":{"display_name":"Conflicting commerce value"}}`), "enterprise-conflict-001", tokenB, fmt.Sprintf(`"%d"`, version), 409)
	again := request("PATCH", path, []byte(`{"set":{"display_name":"Conflicting commerce value"}}`), "enterprise-conflict-001", tokenB, fmt.Sprintf(`"%d"`, version), 409)
	var problem1, problem2 any
	_ = json.Unmarshal(rejected, &problem1)
	_ = json.Unmarshal(again, &problem2)
	if !reflect.DeepEqual(problem1, problem2) {
		t.Fatal("rejected receipt changed")
	}
	if s.count("profiles") != 1 || s.count("outbox") != 4 {
		t.Fatal("ownership rejection changed state/outbox")
	}
	var profile profiles.Profile
	if err = json.Unmarshal(request("GET", path, nil, "", tokenA, "", 200), &profile); err != nil {
		t.Fatal(err)
	}
	shopify := 0
	for _, id := range profile.Identifiers {
		if id.Source == "shopify" {
			shopify++
		}
	}
	if profile.Version != version || shopify != 2 {
		t.Fatal(profile)
	}
	found := false
	for _, p := range profile.Provenance {
		if p.Source == "shopify" && p.Outcome == "rejected" {
			found = true
		}
	}
	if !found {
		t.Fatal("missing rejected provenance")
	}
	request("PATCH", path, []byte(`{"set":{"display_name":"Current synthetic name"}}`), "enterprise-update-0001", tokenA, fmt.Sprintf(`"%d"`, version), 200)
	// Duplicate delivery after subsequent mutations retains the original small receipt.
	replay := request("POST", "/v1/profiles", create, "enterprise-create-0001", tokenA, "", 201)
	var replayReceipt profiles.Receipt
	if err = json.Unmarshal(replay, &replayReceipt); err != nil || replayReceipt != receipt {
		t.Fatal("receipt changed", err)
	}
	request("PATCH", path, []byte(`{"set":{"display_name":"Historical backfill"}}`), "enterprise-backfill-001", tokenA, fmt.Sprintf(`"%d"`, version), 412)
	request("POST", "/v1/profiles", []byte(`{"set":{"display_name":[]}}`), "enterprise-partial-bad", tokenA, "", 422)
	if s.count("profiles") != 1 || s.count("outbox") != 5 {
		t.Fatal("failed batch item changed state")
	}
	// A separate source record with the same email does not cause an automatic merge.
	sf.Identifiers[0].Value = "003000000000999"
	second, err := enterprise.RequestJSON(sf)
	if err != nil {
		t.Fatal(err)
	}
	request("POST", "/v1/profiles", second, "enterprise-partial-good", tokenA, "", 201)
	if s.count("profiles") != 2 || s.count("outbox") != 6 {
		t.Fatal("partial import/independent profile failed")
	}
}
