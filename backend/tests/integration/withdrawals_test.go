package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// noticesFor are the withdrawal notices a receiver got for a record.
func noticesFor(r *receiver, record string) []receivedDelivery {
	var out []receivedDelivery
	for _, d := range sentFor(r, record) {
		if strings.Contains(d.body, `"type":"notification.withdrawn.v1"`) {
			out = append(out, d)
		}
	}
	return out
}

func (s activationSuite) withdrawals(subscription string) []platform.Withdrawal {
	s.t.Helper()
	res := s.request("GET", subscriptionsPath+"/"+subscription+"/withdrawals", "", "", viewOperator, "")
	s.expect(res, 200)
	var page struct{ Items []platform.Withdrawal }
	if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
		s.t.Fatal(err)
	}
	return page.Items
}

// A recipient that was told about a record is told to stop using it once the record is no longer
// authorized for the subscription's purpose: after an objection, a restriction, or a change only the
// sweep sees, and also when the withdrawal raced the notification itself. Each notice is sent once,
// decided again before every attempt, and every attempt is recorded.
func TestRecipientsAreToldWhenARecordIsWithdrawn(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	n := platform.NewNotifier(s.store, s.config.Egress())
	w := platform.NewWithdrawals(s.store, s.config.Egress(), "acme")
	s.destination("crm_changes", r)
	purpose, policy := s.purposeWithBasis("crm_sync", "legitimate_interests")
	s.subscribe("crm_sync", subscriptionBody("crm_changes", purpose, "console"))
	var subscription string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM notification_subscriptions WHERE name = 'crm_sync'`).Scan(&subscription); err != nil {
		t.Fatal(err)
	}
	// deliver notifies the record's newest change, as the profile-change workflow does.
	deliver := func(record string) events.Event {
		t.Helper()
		changes := s.outboxEvents(record)
		e := changes[len(changes)-1]
		result, err := n.Notify(ctx, e, subscription)
		if err != nil || result.Outcome != platform.NotificationDelivered {
			t.Fatalf("notify: %+v %v", result, err)
		}
		if err = n.Record(ctx, e, subscription, result); err != nil {
			t.Fatal(err)
		}
		return e
	}
	pass := func() {
		t.Helper()
		if err := w.Pass(ctx); err != nil {
			t.Fatal(err)
		}
	}

	// An objection after delivery: one signed notice, with references and a reason code only.
	ada := s.viewRecord(tokenA, "withdraw-ada", `{"tier":"gold"}`)
	delivered := deliver(ada)
	pass()
	if len(noticesFor(r, ada)) != 0 {
		t.Fatal("a notice was sent for an authorized record")
	}
	s.recordEvidence(ada, policy, "object")
	pass()
	pass()
	notices := noticesFor(r, ada)
	if len(notices) != 1 || notices[0].signature == "" || strings.Contains(notices[0].body, "gold") {
		t.Fatalf("notices %+v", notices)
	}
	var notice map[string]any
	if err := json.Unmarshal([]byte(notices[0].body), &notice); err != nil {
		t.Fatal(err)
	}
	if notice["schema"] != platform.WithdrawalSchema || notice["subscription"] != "crm_sync" || notice["reason"] != "purpose_not_permitted" ||
		notice["last_profile_version"] != float64(delivered.ProfileVersion) || notice["id"] != notices[0].key || notice["tenant"] != "acme" {
		t.Fatalf("notice %v", notice)
	}

	// A notification in flight when the objection commits: decided before it, recorded after it.
	bob := s.viewRecord(tokenA, "withdraw-bob", `{"tier":"gold"}`)
	changes := s.outboxEvents(bob)
	inflight := changes[len(changes)-1]
	result, err := n.Notify(ctx, inflight, subscription)
	if err != nil || result.Outcome != platform.NotificationDelivered {
		t.Fatalf("notify: %+v %v", result, err)
	}
	s.recordEvidence(bob, policy, "object")
	if err = n.Record(ctx, inflight, subscription, result); err != nil {
		t.Fatal(err)
	}
	pass()
	if len(noticesFor(r, bob)) != 1 {
		t.Fatal("a notification that raced an objection was never withdrawn")
	}

	// A restriction, and one lifted before its notice was sent: that notice is cancelled, not sent.
	cy := s.viewRecord(tokenA, "withdraw-cy", `{"tier":"gold"}`)
	dan := s.viewRecord(tokenA, "withdraw-dan", `{"tier":"gold"}`)
	deliver(cy)
	deliver(dan)
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	for _, record := range []string{cy, dan} {
		s.expect(s.request("POST", restrictionPath(record), restrictBody("accuracy_contested", nextMonth), "", viewOperator, ""), 200)
	}
	if _, err = w.ReviewQueued(ctx); err != nil {
		t.Fatal(err)
	}
	s.expect(s.request("POST", restrictionPath(dan), `{"action":"lift","reason":"claim_resolved"}`, "", viewOperator, ""), 200)
	if _, err = w.SendDue(ctx, 10); err != nil {
		t.Fatal(err)
	}
	if len(noticesFor(r, cy)) != 1 || len(noticesFor(r, dan)) != 0 {
		t.Fatalf("restricted %d, lifted %d", len(noticesFor(r, cy)), len(noticesFor(r, dan)))
	}

	// What no write announces is found by the sweep.
	eve := s.viewRecord(tokenA, "withdraw-eve", `{"tier":"gold"}`)
	deliver(eve)
	pass()
	if _, err = s.pool.Exec(ctx, `UPDATE profiles SET restricted = true WHERE id = $1`, eve); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		pass()
	}
	if len(noticesFor(r, eve)) != 1 {
		t.Fatal("the sweep missed a withdrawn record")
	}

	// The history: each notice once, with its attempts; the cancelled one says why.
	states := map[string]string{}
	for _, item := range s.withdrawals("crm_sync") {
		states[item.ProfileID] = item.State + ":" + item.Reason
		if len(item.Attempts) != 1 {
			t.Fatalf("attempts %+v", item)
		}
		if item.ProfileID == dan && item.Attempts[0].Reason != "authorized_again" {
			t.Fatalf("cancelled %+v", item)
		}
	}
	want := map[string]string{ada: "delivered:purpose_not_permitted", bob: "delivered:purpose_not_permitted",
		cy: "delivered:record_restricted", dan: "cancelled:record_restricted", eve: "delivered:record_restricted"}
	for record, state := range want {
		if states[record] != state {
			t.Fatalf("%s: %s, want %s (%v)", record, states[record], state, states)
		}
	}
	// Recorded again, a delivered notice changes nothing; a later delivery would hold the record anew.
	if _, err = s.pool.Exec(ctx, `INSERT INTO notification_review_queue (profile_id) VALUES ($1), ($2)`, ada, cy); err != nil {
		t.Fatal(err)
	}
	pass()
	if len(s.withdrawals("crm_sync")) != 5 || len(noticesFor(r, ada)) != 1 {
		t.Fatal("a withdrawn record was withdrawn again")
	}
	if _, err = s.pool.Exec(ctx, `UPDATE notification_withdrawal_attempts SET reason = 'x'`); err == nil {
		t.Fatal("withdrawal attempts are not append-only")
	}
}

// A notice the recipient rejects fails; a privacy admin redrives it, and it is decided and sent again.
func TestRejectedWithdrawalNoticesCanBeRedriven(t *testing.T) {
	r := newReceiver(t, 200, 400)
	s := setupActivation(t, r)
	ctx := context.Background()
	n := platform.NewNotifier(s.store, s.config.Egress())
	w := platform.NewWithdrawals(s.store, s.config.Egress(), "acme")
	s.destination("crm_changes", r)
	purpose, policy := s.purposeWithBasis("crm_sync", "legitimate_interests")
	subscription := s.subscribe("crm_sync", subscriptionBody("crm_changes", purpose, "console"))
	record := s.viewRecord(tokenA, "withdraw-rejected", `{"tier":"gold"}`)
	e := s.outboxEvents(record)[0]
	result, err := n.Notify(ctx, e, subscription)
	if err != nil || result.Outcome != platform.NotificationDelivered {
		t.Fatalf("notify %+v %v", result, err)
	}
	if err = n.Record(ctx, e, subscription, result); err != nil {
		t.Fatal(err)
	}
	s.recordEvidence(record, policy, "object")
	if err = w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	items := s.withdrawals("crm_sync")
	if len(items) != 1 || items[0].State != "failed" || items[0].Attempts[0].Reason != "rejected" || *items[0].Attempts[0].StatusCode != 400 {
		t.Fatalf("rejected %+v", items)
	}
	s.expect(s.request("POST", subscriptionsPath+"/crm_sync/withdrawals/redrive", "", "", strings.Repeat("u", 64), ""), 401)
	res := s.request("POST", subscriptionsPath+"/crm_sync/withdrawals/redrive", "", "", viewOperator, "")
	s.expect(res, 200)
	if !strings.Contains(res.Body.String(), `"requeued":1`) {
		t.Fatalf("redrive %s", res.Body.String())
	}
	if err = w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	items = s.withdrawals("crm_sync")
	if items[0].State != "delivered" || len(items[0].Attempts) != 2 || len(noticesFor(r, record)) != 2 || noticesFor(r, record)[0].key != noticesFor(r, record)[1].key {
		t.Fatalf("after redrive %+v", items)
	}
}
