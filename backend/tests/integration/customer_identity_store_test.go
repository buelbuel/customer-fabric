package integration

import (
	"encoding/json"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

func TestResolveCustomerProfileConcurrently(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	store := platform.Store{Pool: s.pool}
	subject := platform.UUID()

	// Source-controlled email and identifiers cannot establish a Kratos link.
	crm := s.request("POST", "/v1/profiles",
		`{"set":{"email":"synthetic@example.com"},"identifiers":[{"namespace":"kratos","value":"`+subject+`"}]}`,
		platform.UUID(), tokenB, "")
	s.expect(crm, 201)
	var crmReceipt profiles.Receipt
	if err := json.Unmarshal(crm.Body.Bytes(), &crmReceipt); err != nil {
		t.Fatal(err)
	}

	const callers = 8
	type resolution struct {
		id  string
		err error
	}
	start := make(chan struct{})
	results := make(chan resolution, callers)
	for i := 0; i < callers; i++ {
		go func() {
			<-start
			id, err := store.ResolveCustomerProfile(ctx, subject)
			results <- resolution{id: id, err: err}
		}()
	}
	close(start)

	var profileID string
	for i := 0; i < callers; i++ {
		result := <-results
		if result.err != nil {
			t.Error("resolve customer profile:", result.err)
			continue
		}
		if profileID == "" {
			profileID = result.id
		}
		if result.id != profileID {
			t.Error("concurrent requests returned different profiles")
		}
	}
	if profileID == "" || profileID == crmReceipt.ID {
		t.Fatal("expected a dedicated customer profile")
	}
	if s.count("customer_identity_links") != 1 || s.count("profiles") != 2 || s.count("outbox") != 1 {
		t.Fatal("concurrent provisioning changed unexpected state")
	}

	customer, err := store.Get(ctx, profileID)
	if err != nil {
		t.Fatal(err)
	}
	if customer.Records[0].Version != 1 || len(customer.Attributes) != 0 || len(customer.Identifiers) != 0 {
		t.Fatal("customer profile contains source data")
	}
	crmProfile, err := store.Get(ctx, crmReceipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if crmProfile.Records[0].Version != 1 || string(crmProfile.Attributes["email"]) != `"synthetic@example.com"` {
		t.Fatal("CRM profile changed during customer provisioning")
	}
}

func TestResolveCustomerProfileRollsBackOnLinkFailure(t *testing.T) {
	s := setup(t)
	ctx := t.Context()
	if _, err := s.pool.Exec(ctx, `ALTER TABLE customer_identity_links ADD CONSTRAINT test_reject_link CHECK (false)`); err != nil {
		t.Fatal(err)
	}
	store := platform.Store{Pool: s.pool}
	if _, err := store.ResolveCustomerProfile(ctx, platform.UUID()); err == nil {
		t.Fatal("expected a link failure")
	}
	if s.count("profiles") != 0 || s.count("customer_identity_links") != 0 {
		t.Fatal("a failed link left partial state")
	}
}
