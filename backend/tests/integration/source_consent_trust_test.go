package integration

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// A source grant is the source's signed attestation (ADR 0017). It is recorded only with a receipt signed
// by an active key of a vetted authority over exactly that decision, and it counts only while the
// authority is active and within its review date and its key is not compromised. Forged, moved, unsigned
// or wrongly keyed receipts are refused; every change to the authority is audited.
func TestSourceConsentReceiptsAreSignedAndAuthoritiesGoverned(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	marketing, policy := s.purposeWithBasis("signed_marketing", "consent")
	subscription := s.subscribe("signed_marketing", subscriptionBody("crm_changes", marketing, "console"))
	n := platform.NewNotifier(platform.Store{Pool: s.pool}, s.config.WebhookHosts)
	version := map[string]int64{}
	notified := func(record string) string {
		t.Helper()
		version[record]++
		got, err := n.Notify(ctx, changeOf(record, version[record]), subscription)
		if err != nil {
			t.Fatal(err)
		}
		if got.Outcome == platform.NotificationDelivered {
			return "delivered"
		}
		return got.Reason
	}
	authorityBody := func(review string) string {
		return fmt.Sprintf(`{"source":"console","purpose_id":%q,"controller":"Synthetic Retail GmbH","collection_methods":["double_opt_in"],%s}`, marketing, review)
	}
	// Vetting is required: the arrangement, the vetting record and a review date within a year.
	for name, review := range map[string]string{
		"no arrangement":   fmt.Sprintf(`"vetting_reference":"VET-1","review_by":%q`, time.Now().AddDate(0, 1, 0).Format(time.DateOnly)),
		"no vetting":       fmt.Sprintf(`"arrangement_reference":"DPA-1","review_by":%q`, time.Now().AddDate(0, 1, 0).Format(time.DateOnly)),
		"review past":      `"arrangement_reference":"DPA-1","vetting_reference":"VET-1","review_by":"2020-01-01"`,
		"review too far":   fmt.Sprintf(`"arrangement_reference":"DPA-1","vetting_reference":"VET-1","review_by":%q`, time.Now().AddDate(2, 0, 0).Format(time.DateOnly)),
		"review malformed": `"arrangement_reference":"DPA-1","vetting_reference":"VET-1","review_by":"soon"`,
	} {
		if w := s.request("POST", consentAuthoritiesPath, authorityBody(review), "", viewOperator, ""); w.Code != 422 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	authority := s.approveAuthority("console", marketing, "double_opt_in")
	keys := consentAuthoritiesPath + "/" + authority + "/keys"
	s.expect(s.request("POST", keys, `{"key_id":"bad key","public_key":"AAAA"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", keys, `{"key_id":"short","public_key":"AAAA"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", keys, fmt.Sprintf(`{"key_id":%q,"public_key":"%s"}`, receiptKeyID, strings.Repeat("A", 43)+"="), "", viewOperator, ""), 409)
	s.expect(s.request("POST", keys, `{}`, "", tokenA, ""), 401)
	s.expect(s.request("GET", keys, "", "", viewOperator, ""), 200)

	record := s.viewRecord(tokenA, "signed-ada", `{"tier":"gold"}`)
	s.viewRecord(tokenA, "signed-bob", `{"tier":"gold"}`)
	grant := func(key string, d sourceDecision) int {
		t.Helper()
		d.record, d.policy = "signed-ada", policy
		return s.reportConsent(tokenA, key, d).Code
	}
	// Unsigned, unknown key, forged, or signed over another decision: refused, nothing recorded.
	other := ed25519.NewKeyFromSeed([]byte("synthetic-attacker-receipt-key-9"))
	for name, c := range map[string]struct {
		d      sourceDecision
		status int
	}{
		"unsigned":                {sourceDecision{unsigned: true}, 422},
		"unknown key":             {sourceDecision{keyID: "console-unknown"}, 403},
		"forged":                  {sourceDecision{signer: other}, 403},
		"moved to another record": {sourceDecision{tamper: func(d *platform.SourceConsentDecision) { d.Record.Value = "signed-bob" }}, 403},
		"another proof":           {sourceDecision{tamper: func(d *platform.SourceConsentDecision) { d.Proof.SHA256 = strings.Repeat("ab", 32) }}, 403},
	} {
		if got := grant("refused-"+strings.ReplaceAll(name, " ", "-")+"-0001", c.d); got != c.status {
			t.Errorf("%s: %d", name, got)
		}
	}
	if got := notified(record); got != "purpose_not_permitted" {
		t.Fatalf("a refused receipt counted: %s", got)
	}

	// A properly signed grant counts and is marked as the source's attestation.
	res := s.reportConsent(tokenA, "signed-grant-00000001", sourceDecision{record: "signed-ada", policy: policy})
	s.expect(res, 201)
	if !strings.Contains(res.Body.String(), `"trust":"source_attested"`) {
		t.Fatalf("receipt: %s", res.Body.String())
	}
	if got := notified(record); got != "delivered" {
		t.Fatalf("a signed grant: %s", got)
	}

	// Rotation: a new key signs new grants; a retired key signs none, but its grants keep counting.
	next := ed25519.NewKeyFromSeed([]byte("synthetic-console-receipt-key-02"))
	s.addReceiptKey(authority, "console-2026-2", next)
	s.expect(s.request("POST", keys+"/"+receiptKeyID+"/retire", "", "", viewOperator, ""), 200)
	s.expect(s.request("POST", keys+"/"+receiptKeyID+"/retire", "", "", viewOperator, ""), 409)
	if got := grant("retired-key-grant-0001", sourceDecision{}); got != 403 {
		t.Fatalf("a retired key signed a new grant: %d", got)
	}
	if got := notified(record); got != "delivered" {
		t.Fatalf("a retired key's grant: %s", got)
	}
	// Compromise: every grant the key signed stops counting at once; the new key's grants count.
	s.expect(s.request("POST", keys+"/"+receiptKeyID+"/compromise", "", "", viewOperator, ""), 200)
	s.expect(s.request("POST", keys+"/"+receiptKeyID+"/compromise", "", "", viewOperator, ""), 409)
	s.expect(s.request("POST", keys+"/console-unknown/compromise", "", "", viewOperator, ""), 404)
	if got := notified(record); got != "purpose_not_permitted" {
		t.Fatalf("a compromised key's grant still counts: %s", got)
	}
	if got := grant("rotated-key-grant-0001", sourceDecision{keyID: "console-2026-2", signer: next}); got != 201 {
		t.Fatalf("the new key: %d", got)
	}
	if got := notified(record); got != "delivered" {
		t.Fatalf("after re-granting with the new key: %s", got)
	}

	// Suspension stops the authority's grants from counting and refuses new ones; renewal restores them.
	authorityPath := consentAuthoritiesPath + "/" + authority
	s.expect(s.request("POST", authorityPath+"/suspend", `{"reference":""}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", authorityPath+"/suspend", `{"reference":"INC-2026-044"}`, "", tokenA, ""), 401)
	s.expect(s.request("POST", authorityPath+"/suspend", `{"reference":"INC-2026-044"}`, "", viewOperator, ""), 200)
	s.expect(s.request("POST", authorityPath+"/suspend", `{"reference":"INC-2026-044"}`, "", viewOperator, ""), 409)
	if got := notified(record); got != "purpose_not_permitted" {
		t.Fatalf("a suspended authority's grant: %s", got)
	}
	if got := grant("suspended-grant-00001", sourceDecision{keyID: "console-2026-2", signer: next}); got != 403 {
		t.Fatalf("a suspended authority took a grant: %d", got)
	}
	s.expect(s.request("POST", authorityPath+"/renew", "{"+vetting()+"}", "", viewOperator, ""), 200)
	if got := notified(record); got != "delivered" {
		t.Fatalf("after renewal: %s", got)
	}

	// Past its review date the authority authorizes nothing until it is renewed.
	if _, err := s.pool.Exec(ctx, `UPDATE consent_authorities SET review_by = current_date - 1 WHERE id = $1`, authority); err != nil {
		t.Fatal(err)
	}
	if got := notified(record); got != "purpose_not_permitted" {
		t.Fatalf("an authority past review: %s", got)
	}
	if got := grant("overdue-grant-0000001", sourceDecision{keyID: "console-2026-2", signer: next}); got != 403 {
		t.Fatalf("an authority past review took a grant: %d", got)
	}
	s.expect(s.request("POST", authorityPath+"/renew", "{"+vetting()+"}", "", viewOperator, ""), 200)

	// Withdrawals never need a receipt or a key, even once the authority is revoked.
	s.expect(s.request("POST", authorityPath+"/revoke", "", "", viewOperator, ""), 200)
	s.expect(s.request("POST", authorityPath+"/renew", "{"+vetting()+"}", "", viewOperator, ""), 409)
	s.expect(s.request("POST", keys, fmt.Sprintf(`{"key_id":"late","public_key":%q}`, "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="), "", viewOperator, ""), 409)
	s.expect(s.reportConsent(tokenA, "unsigned-withdrawal-01", sourceDecision{record: "signed-ada", policy: policy, decision: "withdraw", unsigned: true}), 201)

	// Every change is audited with its actor.
	audit := s.request("GET", authorityPath+"/audit", "", "", viewOperator, "")
	s.expect(audit, 200)
	var trail struct {
		Items []platform.AuthorityAuditEntry
	}
	if err := json.Unmarshal(audit.Body.Bytes(), &trail); err != nil {
		t.Fatal(err)
	}
	var actions []string
	for _, entry := range trail.Items {
		actions = append(actions, entry.Action)
		if entry.Actor == "" {
			t.Fatalf("an audit entry without actor: %+v", entry)
		}
	}
	want := "approved key_added key_added key_retired key_compromised suspended resumed renewed revoked"
	if got := strings.Join(actions, " "); got != want {
		t.Fatalf("audit: %s, want %s", got, want)
	}
	s.expect(s.request("GET", authorityPath+"/audit", "", "", tokenA, ""), 401)
	s.expect(s.request("GET", consentAuthoritiesPath+"/"+platform.UUID()+"/audit", "", "", viewOperator, ""), 404)
}
