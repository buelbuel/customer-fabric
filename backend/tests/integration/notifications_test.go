package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const subscriptionsPath = "/v1/operator/notification-subscriptions"

// purposeWithBasis creates a purpose whose active policy version records the given lawful basis.
func (s linkingSuite) purposeWithBasis(name, basis string) (purpose, policy string) {
	s.t.Helper()
	res := s.request("POST", "/v1/operator/consent/purposes", `{"name":"`+name+`"}`, "", viewOperator, "")
	s.expect(res, 201)
	var p struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &p)
	res = s.request("POST", "/v1/operator/consent/policies/"+p.ID, `{"explanation":"Synthetic explanation.","lawful_basis":"`+basis+`","notice_reference":"n","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`, "", viewOperator, "")
	s.expect(res, 201)
	var v struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &v)
	s.expect(s.request("POST", "/v1/operator/consent/policy-versions/"+v.ID+"/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	return p.ID, v.ID
}

func subscriptionBody(destination, purpose string, sources ...string) string {
	quoted := make([]string, len(sources))
	for i, source := range sources {
		quoted[i] = fmt.Sprintf("%q", source)
	}
	return fmt.Sprintf(`{"destination":%q,"purpose_id":%q,"basis_assessment":"LIA-2026-017","recipient_role":"processor","event_types":["profile.created.v1","profile.updated.v1"],"data_categories":["record_reference"],"sources":[%s]}`,
		destination, purpose, strings.Join(quoted, ","))
}

func (s linkingSuite) subscribe(name, body string) string {
	s.t.Helper()
	s.expect(s.request("POST", subscriptionsPath+"/"+name, body, "", viewOperator, ""), 201)
	var id string
	if err := s.pool.QueryRow(context.Background(), `SELECT id FROM notification_subscriptions WHERE name = $1`, name).Scan(&id); err != nil {
		s.t.Fatal(err)
	}
	return id
}

// recordEvidence records an operator decision on a source record's own subject.
func (s linkingSuite) recordEvidence(record, policy, decision string) {
	s.t.Helper()
	var epoch string
	if err := s.pool.QueryRow(context.Background(), `SELECT record_epoch FROM external_identifiers WHERE profile_id = $1 LIMIT 1`, record).Scan(&epoch); err != nil {
		s.t.Fatal(err)
	}
	body := fmt.Sprintf(`{"policy_version_id":%q,"decision":%q,"occurred_at":%q,"proof_channel":"operator_console","proof_reference":"synthetic-ref"}`,
		policy, decision, time.Now().UTC().Format(time.RFC3339))
	s.expect(s.request("POST", "/v1/operator/consent/evidence/record/"+epoch, body, "evidence-"+platform.UUID(), viewOperator, ""), 201)
}

func changeOf(record string, version int64) events.Event {
	return events.Event{Schema: events.Schema, ID: platform.UUID(), Type: "profile.updated.v1", Tenant: "acme", Source: "console",
		ProfileID: record, ProfileVersion: version, OccurredAt: time.Now().UTC()}
}

// A subscription is the authorization behind every notification: it can only be created with a
// documented lawful basis, names references as the only data it may receive, and is scoped to sources.
func TestSubscriptionsNeedADocumentedBasis(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("crm_sync", "legitimate_interests")
	undocumented := struct{ ID string }{}
	res := s.request("POST", "/v1/operator/consent/purposes", `{"name":"undocumented"}`, "", viewOperator, "")
	s.expect(res, 201)
	_ = json.Unmarshal(res.Body.Bytes(), &undocumented)

	for name, body := range map[string]string{
		"no lawful basis":   subscriptionBody("crm_changes", undocumented.ID, "console"),
		"unknown purpose":   subscriptionBody("crm_changes", platform.UUID(), "console"),
		"unknown target":    subscriptionBody("missing_destination", purpose, "console"),
		"unknown source":    subscriptionBody("crm_changes", purpose, "nowhere"),
		"no sources":        subscriptionBody("crm_changes", purpose),
		"attribute values":  strings.Replace(subscriptionBody("crm_changes", purpose, "console"), `["record_reference"]`, `["record_reference","attributes"]`, 1),
		"unknown event":     strings.Replace(subscriptionBody("crm_changes", purpose, "console"), `"profile.updated.v1"`, `"profile.deleted.v1"`, 1),
		"no recipient role": strings.Replace(subscriptionBody("crm_changes", purpose, "console"), `"processor"`, `"partner"`, 1),
		// A non-consent basis authorizes nothing without the approved assessment behind it.
		"no assessment":  strings.Replace(subscriptionBody("crm_changes", purpose, "console"), `"basis_assessment":"LIA-2026-017",`, "", 1),
		"bad assessment": strings.Replace(subscriptionBody("crm_changes", purpose, "console"), `LIA-2026-017`, `see notes`, 1),
	} {
		if w := s.request("POST", subscriptionsPath+"/crm", body, "", viewOperator, ""); w.Code != 422 {
			t.Errorf("%s: %d %s", name, w.Code, w.Body.String())
		}
	}
	s.expect(s.request("POST", subscriptionsPath+"/crm", subscriptionBody("crm_changes", purpose, "console"), "", tokenA, ""), 401)
	s.subscribe("crm", subscriptionBody("crm_changes", purpose, "console"))
	s.expect(s.request("POST", subscriptionsPath+"/crm", subscriptionBody("crm_changes", purpose, "console"), "", viewOperator, ""), 409)
	s.expect(s.request("POST", subscriptionsPath+"/crm/state", `{"state":"suspended"}`, "", viewOperator, ""), 200)
	s.expect(s.request("POST", subscriptionsPath+"/crm/state", `{"state":"paused"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", subscriptionsPath+"/unknown/state", `{"state":"active"}`, "", viewOperator, ""), 404)
	s.expect(s.request("POST", subscriptionsPath+"/crm/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	listed := s.request("GET", subscriptionsPath, "", "", viewOperator, "")
	s.expect(listed, 200)
	if !strings.Contains(listed.Body.String(), `"recipient_role":"processor"`) || strings.Contains(listed.Body.String(), "signing_secret") {
		t.Fatalf("list %s", listed.Body.String())
	}

	// Only privacy_admin may authorize external processing.
	issuer := newRolesIssuer(t, "profile_reader", "activation_operator")
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{Mode: "development", OperatorKratosPublicURL: issuer.url,
		KratosPublicURL: "http://customer-kratos.invalid:4433", Tokens: map[string]string{"console": tokenA, "importer": tokenB}})
	if err != nil {
		t.Fatal(err)
	}
	if w := operatorSession(h, "POST", subscriptionsPath+"/other", subscriptionBody("crm_changes", purpose, "console")); w.Code != 403 {
		t.Fatalf("activation_operator created a subscription: %d", w.Code)
	}
	if w := operatorSession(h, "GET", subscriptionsPath, ""); w.Code != 200 {
		t.Fatalf("profile_reader could not list subscriptions: %d", w.Code)
	}
}

// The notifier authorizes every attempt from current state: subscription scope, the record's lifecycle
// and restriction, the purpose's lawful basis and the subject's decisions under it, and the destination.
func TestNotifierAuthorizesEveryAttempt(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	store := s.store
	secret := s.destination("crm_changes", r)
	n := platform.NewNotifier(store, s.config.Egress())
	notify := func(e events.Event, subscription string) platform.NotificationResult {
		t.Helper()
		got, err := n.Notify(ctx, e, subscription)
		if err != nil {
			t.Fatalf("notify: %v", err)
		}
		return got
	}
	expectSuppressed := func(e events.Event, subscription, reason string) {
		t.Helper()
		if got := notify(e, subscription); got.Outcome != platform.NotificationSuppressed || got.Reason != reason {
			t.Fatalf("want suppressed %s, got %+v", reason, got)
		}
	}

	// Legitimate interests: permitted for an unlinked record until its subject objects.
	interest, interestPolicy := s.purposeWithBasis("crm_sync", "legitimate_interests")
	sync := s.subscribe("crm_sync", subscriptionBody("crm_changes", interest, "console"))
	unlinked := s.viewRecord(tokenA, "notify-unlinked", `{"tier":"gold"}`)
	e := changeOf(unlinked, 1)
	if subs, err := n.Subscriptions(ctx, e); err != nil || len(subs) != 1 || subs[0] != sync {
		t.Fatalf("subscriptions %v %v", subs, err)
	}
	got := notify(e, sync)
	if got.Outcome != platform.NotificationDelivered || got.StatusCode != 200 {
		t.Fatalf("legitimate interests: %+v", got)
	}
	sent := r.deliveries()
	if len(sent) != 1 || sent[0].key != e.ID || strings.Contains(sent[0].body, "gold") {
		t.Fatalf("sent %+v", sent)
	}
	if !signedBy(sent[0].signature, sent[0].body, secret) {
		t.Fatal("the signature does not verify")
	}
	if err := n.Record(ctx, e, sync, platform.NotificationResult{Outcome: got.Outcome, Reason: got.Reason, StatusCode: got.StatusCode, Attempts: 1}); err != nil {
		t.Fatal(err)
	}
	if again := notify(e, sync); again.Reason != "already_recorded" || len(r.deliveries()) != 1 {
		t.Fatalf("a recorded event was sent again: %+v", again)
	}
	s.recordEvidence(unlinked, interestPolicy, "object")
	expectSuppressed(changeOf(unlinked, 2), sync, "purpose_not_permitted")

	// Consent: an unlinked record has nobody who could consent, so it is never notified, and an operator's
	// grant on the record does not count as consent.
	marketing, marketingPolicy := s.purposeWithBasis("crm_marketing", "consent")
	consented := s.subscribe("crm_marketing", subscriptionBody("crm_changes", marketing, "console"))
	other := s.viewRecord(tokenA, "notify-other", `{"tier":"gold"}`)
	expectSuppressed(changeOf(other, 1), consented, "purpose_not_permitted")
	s.recordEvidence(other, marketingPolicy, "grant")
	expectSuppressed(changeOf(other, 1), consented, "purpose_not_permitted")

	// Consent: a record linked to a customer follows the customer's own current decision.
	linked := s.viewRecord(tokenA, "notify-linked", `{"tier":"gold"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "notify-linked")
	expectSuppressed(changeOf(linked, 1), consented, "purpose_not_permitted")
	s.decideAsCustomer(linkCustomerA, marketing, marketingPolicy, "grant")
	if got := notify(changeOf(linked, 1), consented); got.Outcome != platform.NotificationDelivered {
		t.Fatalf("after the customer's grant: %+v", got)
	}
	s.decideAsCustomer(linkCustomerA, marketing, marketingPolicy, "withdraw")
	expectSuppressed(changeOf(linked, 2), consented, "purpose_not_permitted")
	s.decideAsCustomer(linkCustomerA, marketing, marketingPolicy, "grant")
	// A restricted customer is never notified about, whatever they consented to.
	anchor := s.anchor(linkCustomerA)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	s.expect(s.request("POST", restrictionPath(anchor), restrictBody("objection_pending", nextMonth), "", viewOperator, ""), 200)
	expectSuppressed(changeOf(linked, 2), consented, "customer_unavailable")
	s.expect(s.request("POST", restrictionPath(anchor), `{"action":"lift","reason":"objection_decided"}`, "", viewOperator, ""), 200)

	// Scope: suspension, event types and sources the subscription does not cover.
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync/state", `{"state":"suspended"}`, "", viewOperator, ""), 200)
	fresh := s.viewRecord(tokenA, "notify-fresh", `{"tier":"gold"}`)
	expectSuppressed(changeOf(fresh, 1), sync, "subscription_suspended")
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	foreign := changeOf(fresh, 1)
	foreign.Source = "importer"
	expectSuppressed(foreign, sync, "source_not_in_scope")
	created := changeOf(fresh, 1)
	createdOnly := s.subscribe("crm_created", strings.Replace(subscriptionBody("crm_changes", interest, "console"), `,"profile.updated.v1"`, "", 1))
	created.Type = "profile.updated.v1"
	expectSuppressed(created, createdOnly, "event_type_not_permitted")
	expectSuppressed(changeOf(fresh, 1), platform.UUID(), "subscription_removed")

	// The record itself: restricted, or no longer active.
	s.expect(s.request("POST", restrictionPath(fresh), restrictBody("accuracy_contested", nextMonth), "", viewOperator, ""), 200)
	expectSuppressed(changeOf(fresh, 1), sync, "record_restricted")
	s.expect(s.request("POST", restrictionPath(fresh), `{"action":"lift","reason":"accuracy_verified"}`, "", viewOperator, ""), 200)
	expectSuppressed(changeOf(platform.UUID(), 1), sync, "record_unavailable")

	// The lawful basis: retiring the policy version the subscriptions were approved under interrupts them,
	// and a new version authorizes nothing until a subscription is approved under it.
	s.retirePolicy(interestPolicy)
	expectSuppressed(changeOf(fresh, 1), sync, "subscription_needs_reapproval")
	res := s.request("POST", "/v1/operator/consent/policies/"+interest, `{"explanation":"Synthetic explanation.","lawful_basis":"legitimate_interests","notice_reference":"n2","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`, "", viewOperator, "")
	s.expect(res, 201)
	var next struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &next)
	s.expect(s.request("POST", "/v1/operator/consent/policy-versions/"+next.ID+"/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	expectSuppressed(changeOf(fresh, 1), sync, "subscription_needs_reapproval")
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync/state", `{"state":"active"}`, "", viewOperator, ""), 409)
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync/state", `{"state":"suspended"}`, "", viewOperator, ""), 200)
	reapproved := s.subscribe("crm_sync_v2", subscriptionBody("crm_changes", interest, "console"))
	if got := notify(changeOf(fresh, 1), reapproved); got.Outcome != platform.NotificationDelivered {
		t.Fatalf("under the re-approved subscription: %+v", got)
	}
}

// The destination's failures are classified for the workflow, and a send cut short by our own deadline
// is ours, not the destination's.
func TestNotifierClassifiesDestinationFailures(t *testing.T) {
	r := newReceiver(t, 500, 400)
	s := setupActivation(t, r)
	ctx := context.Background()
	store := s.store
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("crm_sync", "legitimate_interests")
	sync := s.subscribe("crm_sync", subscriptionBody("crm_changes", purpose, "console"))
	record := s.viewRecord(tokenA, "classify-ada", `{"tier":"gold"}`)
	n := platform.NewNotifier(store, s.config.Egress())
	if _, err := n.Notify(ctx, changeOf(record, 1), sync); !errors.Is(err, platform.ErrNotificationUnavailable) {
		t.Fatalf("a 500: %v", err)
	}
	if _, err := n.Notify(ctx, changeOf(record, 1), sync); !errors.Is(err, platform.ErrNotificationRejected) {
		t.Fatalf("a 400: %v", err)
	}
	if got, err := platform.NewNotifier(store, platform.Egress{Hosts: []string{"elsewhere.invalid:443"}}).Notify(ctx, changeOf(record, 1), sync); err != nil || got.Reason != "destination_not_allowed" {
		t.Fatalf("a destination off the allowlist: %+v %v", got, err)
	}
	// Under the default egress policy an allowlisted host that resolves to a private address is refused
	// when connecting, and nothing is sent; the refusal is final, not retried.
	sent := len(r.deliveries())
	strict := platform.Egress{Hosts: s.config.WebhookHosts, AllowPlaintext: true}
	if got, err := platform.NewNotifier(store, strict).Notify(ctx, changeOf(record, 1), sync); err != nil ||
		got.Outcome != platform.NotificationFailed || got.Reason != "destination_address_refused" || len(r.deliveries()) != sent {
		t.Fatalf("a destination on a private address: %+v %v", got, err)
	}
	if got, _ := platform.NewNotifier(store, platform.Egress{Hosts: s.config.WebhookHosts, AllowPrivate: true}).Notify(ctx, changeOf(record, 1), sync); got.Reason != "destination_not_allowed" {
		t.Fatalf("an http destination without plaintext allowed: %+v", got)
	}

	// The body is read first: only then does the server notice the client hanging up.
	hanging := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer hanging.Close()
	hangingHost := strings.TrimPrefix(hanging.URL, "http://")
	if _, err := store.CreateDestination(ctx, "slow_changes", hanging.URL+"/hook", platform.Egress{Hosts: []string{hangingHost}, AllowPrivate: true, AllowPlaintext: true}); err != nil {
		t.Fatal(err)
	}
	slow := s.subscribe("slow_sync", subscriptionBody("slow_changes", purpose, "console"))
	deadline, stop := context.WithTimeout(ctx, 300*time.Millisecond)
	_, err := platform.NewNotifier(store, platform.Egress{Hosts: []string{hangingHost}, AllowPrivate: true, AllowPlaintext: true}).Notify(deadline, changeOf(record, 1), slow)
	stop()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a send cut short by our deadline: %v", err)
	}
}

// identifierEvidence records an operator decision on the record subject of one identifier's lifetime.
func (s linkingSuite) identifierEvidence(value, policy, decision string) {
	s.t.Helper()
	var epoch string
	if err := s.pool.QueryRow(context.Background(), `SELECT record_epoch FROM external_identifiers WHERE source = 'console' AND value = $1`, value).Scan(&epoch); err != nil {
		s.t.Fatal(err)
	}
	body := fmt.Sprintf(`{"policy_version_id":%q,"decision":%q,"occurred_at":%q,"proof_channel":"operator_console","proof_reference":"synthetic-ref"}`,
		policy, decision, time.Now().UTC().Format(time.RFC3339))
	s.expect(s.request("POST", "/v1/operator/consent/evidence/record/"+epoch, body, "evidence-"+platform.UUID(), viewOperator, ""), 201)
}

// Every record lifetime must permit the purpose: a customer's consent through one identifier does not
// authorize a record whose other identifier nobody consented for, and a customer-linked identifier does
// not mask an objection on another.
func TestNotifierDecidesEveryRecordLifetime(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	n := platform.NewNotifier(s.store, s.config.Egress())
	reason := func(e events.Event, subscription string) string {
		t.Helper()
		got, err := n.Notify(ctx, e, subscription)
		if err != nil {
			t.Fatalf("notify: %v", err)
		}
		if got.Outcome == platform.NotificationDelivered {
			return "delivered"
		}
		return got.Reason
	}

	mixed := s.viewRecord(tokenA, "mixed-ada", `{"tier":"gold"}`, "mixed-ada-alias")
	s.viewLink(linkIssuerA, linkCustomerA, "mixed-ada")
	marketing, marketingPolicy := s.purposeWithBasis("mixed_marketing", "consent")
	consented := s.subscribe("mixed_marketing", subscriptionBody("crm_changes", marketing, "console"))
	s.decideAsCustomer(linkCustomerA, marketing, marketingPolicy, "grant")
	if got := reason(changeOf(mixed, 1), consented); got != "purpose_not_permitted" {
		t.Fatalf("consent through one identifier authorized the whole record: %s", got)
	}

	interest, interestPolicy := s.purposeWithBasis("mixed_sync", "legitimate_interests")
	sync := s.subscribe("mixed_sync", subscriptionBody("crm_changes", interest, "console"))
	if got := reason(changeOf(mixed, 1), sync); got != "delivered" {
		t.Fatalf("legitimate interests without objection: %s", got)
	}
	s.identifierEvidence("mixed-ada-alias", interestPolicy, "object")
	if got := reason(changeOf(mixed, 2), sync); got != "purpose_not_permitted" {
		t.Fatalf("an objection on the unlinked identifier was masked: %s", got)
	}

	// A linked customer's objection under legitimate interests stops it too. The customer API offers no
	// choice for a non-consent purpose, so the objection is recorded as operator evidence on the customer.
	other := s.viewRecord(tokenA, "object-bob", `{"tier":"gold"}`)
	s.viewLink(linkIssuerA, linkCustomerB, "object-bob")
	if got := reason(changeOf(other, 1), sync); got != "delivered" {
		t.Fatalf("before the customer objected: %s", got)
	}
	objection := fmt.Sprintf(`{"policy_version_id":%q,"decision":"object","occurred_at":%q,"proof_channel":"operator_console","proof_reference":"synthetic-ref"}`,
		interestPolicy, time.Now().UTC().Format(time.RFC3339))
	s.expect(s.request("POST", "/v1/operator/consent/evidence/customer/"+s.anchor(linkCustomerB), objection, "evidence-"+platform.UUID(), viewOperator, ""), 201)
	if got := reason(changeOf(other, 2), sync); got != "purpose_not_permitted" {
		t.Fatalf("after the customer objected: %s", got)
	}

	// A record pending erasure, and a record subject that is no longer active.
	pending := s.viewRecord(tokenA, "pending-cy", `{"tier":"gold"}`)
	if _, err := s.pool.Exec(ctx, `UPDATE profiles SET lifecycle_state = 'erasure_pending' WHERE id = $1`, pending); err != nil {
		t.Fatal(err)
	}
	if got := reason(changeOf(pending, 1), sync); got != "record_unavailable" {
		t.Fatalf("a record pending erasure: %s", got)
	}
	inactive := s.viewRecord(tokenA, "inactive-dee", `{"tier":"gold"}`)
	s.identifierEvidence("inactive-dee", interestPolicy, "grant")
	if _, err := s.pool.Exec(ctx, `UPDATE consent_subjects SET lifecycle_state = 'erasure_pending'
		WHERE kind = 'source_record' AND anchor_record_epoch = (SELECT record_epoch FROM external_identifiers WHERE value = 'inactive-dee')`); err != nil {
		t.Fatal(err)
	}
	if got := reason(changeOf(inactive, 1), sync); got != "record_subject_unavailable" {
		t.Fatalf("an inactive record subject: %s", got)
	}

	// More evidence than can be evaluated safely fails closed.
	crowded := s.viewRecord(tokenA, "crowded-eve", `{"tier":"gold"}`)
	s.identifierEvidence("crowded-eve", interestPolicy, "grant")
	if _, err := s.pool.Exec(ctx, `INSERT INTO consent_evidence (id, subject_id, policy_version_id, decision, actor_kind, occurred_at, proof_channel, proof_reference, idempotency_key)
		SELECT gen_random_uuid(), s.id, $1, 'grant', 'operator', now(), 'operator_console', 'synthetic-ref', 'bulk-evidence-' || g
		FROM consent_subjects s, generate_series(1, 501) g
		WHERE s.kind = 'source_record' AND s.anchor_record_epoch = (SELECT record_epoch FROM external_identifiers WHERE value = 'crowded-eve')`, interestPolicy); err != nil {
		t.Fatal(err)
	}
	if got := reason(changeOf(crowded, 1), sync); got != "purpose_not_permitted" {
		t.Fatalf("evidence above the limit: %s", got)
	}
}

// A record created without any identifier has no lifetime to authorize, so it is never notified.
func TestNotifierRefusesRecordsWithoutIdentifiers(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("bare_sync", "legitimate_interests")
	sync := s.subscribe("bare_sync", subscriptionBody("crm_changes", purpose, "console"))
	res := s.request("POST", "/v1/profiles", `{"set":{"tier":"gold"}}`, platform.UUID(), tokenA, "")
	s.expect(res, 201)
	var receipt struct{ ID string }
	if err := json.Unmarshal(res.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	got, err := platform.NewNotifier(s.store, s.config.Egress()).Notify(context.Background(), changeOf(receipt.ID, 1), sync)
	if err != nil || got.Outcome != platform.NotificationSuppressed || got.Reason != "record_without_identifier" {
		t.Fatalf("a record without identifiers: %+v %v", got, err)
	}
}
