package integration

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// Eventing and notification data is pruned at its configured age, in batches with receipts, without
// breaking what the rows still guarantee: a record a recipient holds stays held until a notice covers
// it, a covered hold never reopens, pending and failed notices stay, queued recoveries stay, and
// unrelayed outbox rows wait for a configured relay.
func TestEventingRetentionPrunesOnlyWhatIsNoLongerNeeded(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	n := platform.NewNotifier(s.store, s.config.Egress())
	w := platform.NewWithdrawals(s.store, s.config.Egress(), "acme")
	s.destination("crm_changes", r)
	purpose, policy := s.purposeWithBasis("crm_sync", "legitimate_interests")
	subscription := s.subscribe("crm_sync", subscriptionBody("crm_changes", purpose, "console"))
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.pool.Exec(ctx, query, args...); err != nil {
			t.Fatal(query, err)
		}
	}
	deliver := func(record string) {
		t.Helper()
		changes := s.outboxEvents(record)
		e := changes[len(changes)-1]
		result, err := n.Notify(ctx, e, subscription)
		if err != nil || result.Outcome != platform.NotificationDelivered {
			t.Fatalf("notify %+v %v", result, err)
		}
		if err = n.Record(ctx, e, subscription, result); err != nil {
			t.Fatal(err)
		}
	}
	count := func(query string, args ...any) int {
		t.Helper()
		var c int
		if err := s.pool.QueryRow(ctx, query, args...).Scan(&c); err != nil {
			t.Fatal(query, err)
		}
		return c
	}

	// held: two versions delivered, the hold still open. withdrawn: delivered, then withdrawn by notice.
	held := s.viewRecord(tokenA, "retain-held", `{"tier":"gold"}`)
	deliver(held)
	s.expect(s.request("PATCH", "/v1/profiles/"+held, `{"set":{"tier":"silver"}}`, platform.UUID(), tokenA, `"1"`), 200)
	deliver(held)
	withdrawn := s.viewRecord(tokenA, "retain-withdrawn", `{"tier":"gold"}`)
	deliver(withdrawn)
	s.recordEvidence(withdrawn, policy, "object")
	if err := w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	// A failed notice for a third record, which must survive any age.
	failed := s.viewRecord(tokenA, "retain-failed", `{"tier":"gold"}`)
	deliver(failed)
	exec(`INSERT INTO notification_withdrawals (id, subscription_id, profile_id, held_version, reason, state, settled_at)
	  VALUES (gen_random_uuid(), $1, $2, 1, 'record_restricted', 'failed', now())`, subscription, failed)
	// Two recovery requests: one settled, one with an item still queued.
	settled, queued := platform.UUID(), platform.UUID()
	exec(`INSERT INTO notification_recoveries (id, subscription_id, idempotency_key, requested_by, more_pending) VALUES
	  ($1, $3, 'retention-settled-0001', 'synthetic', false), ($2, $3, 'retention-queued-00001', 'synthetic', false)`, settled, queued, subscription)
	exec(`INSERT INTO notification_recovery_items (subscription_id, event_id, recovery_id, profile_id, profile_version, state, reason) VALUES
	  ($1, gen_random_uuid(), $2, $4, 1, 'unrecoverable', 'history_unavailable'),
	  ($1, gen_random_uuid(), $3, $4, 1, 'queued', NULL)`, subscription, settled, queued, held)

	// Everything is now older than any retention; one outbox row was never relayed.
	exec(`UPDATE outbox SET occurred_at = now() - interval '400 days', relayed_at = now() - interval '400 days'`)
	unrelayed := s.viewRecord(tokenA, "retain-unrelayed", `{"tier":"gold"}`)
	exec(`UPDATE outbox SET occurred_at = now() - interval '400 days' WHERE profile_id = $1`, unrelayed)
	exec(`UPDATE notification_deliveries SET recorded_at = now() - interval '400 days'`)
	exec(`UPDATE notification_withdrawals SET created_at = now() - interval '400 days', settled_at = now() - interval '400 days'`)
	exec(`UPDATE notification_recoveries SET requested_at = now() - interval '400 days'`)
	fresh := s.viewRecord(tokenA, "retain-fresh", `{"tier":"gold"}`)

	retention := platform.Retention{Relay: true}.WithDefaults()
	pruned, err := s.store.PruneEventing(ctx, retention, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	// The open hold keeps only its newest version; the withdrawn record's outcomes go, and so does the
	// notice that covered them, in the same pass or the next.
	if count(`SELECT count(*) FROM notification_deliveries WHERE profile_id = $1`, held) != 1 ||
		count(`SELECT count(*) FROM notification_deliveries WHERE profile_id = $1 AND profile_version = 2`, held) != 1 {
		t.Fatal("the open hold lost its newest delivery or kept an older one")
	}
	if count(`SELECT count(*) FROM notification_deliveries WHERE profile_id = $1`, withdrawn) != 0 {
		t.Fatal("a withdrawn record's outcomes were kept")
	}
	if _, err = s.store.PruneEventing(ctx, retention, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM notification_withdrawals WHERE profile_id = $1`, withdrawn) != 0 ||
		count(`SELECT count(*) FROM notification_withdrawal_attempts`) != 0 {
		t.Fatal("a covered notice or its attempts were kept")
	}
	if count(`SELECT count(*) FROM notification_withdrawals WHERE state = 'failed'`) != 1 {
		t.Fatal("a failed notice was pruned")
	}
	// The withdrawn record is not held again, so no second notice; the open hold still is.
	if err = w.Pass(ctx); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM notification_withdrawals WHERE profile_id = $1`, withdrawn) != 0 {
		t.Fatal("a pruned hold reopened")
	}
	if count(`SELECT count(*) FROM notification_recoveries WHERE id = $1`, settled) != 0 ||
		count(`SELECT count(*) FROM notification_recoveries WHERE id = $1`, queued) != 1 {
		t.Fatal("recoveries pruned wrongly")
	}
	if count(`SELECT count(*) FROM outbox WHERE profile_id = $1`, unrelayed) != 1 || count(`SELECT count(*) FROM outbox WHERE profile_id = $1`, fresh) != 1 ||
		count(`SELECT count(*) FROM outbox WHERE profile_id = $1`, held) != 0 {
		t.Fatal("outbox pruned wrongly with a relay")
	}
	// Without a relay, unrelayed rows are never published and age out too.
	if _, err = s.store.PruneEventing(ctx, platform.Retention{}.WithDefaults(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if count(`SELECT count(*) FROM outbox WHERE profile_id = $1`, unrelayed) != 0 || count(`SELECT count(*) FROM outbox WHERE profile_id = $1`, fresh) != 1 {
		t.Fatal("outbox pruned wrongly without a relay")
	}
	if pruned["outbox"] == 0 || pruned["notification_outcomes"] == 0 || pruned["notification_recoveries"] != 1 {
		t.Fatalf("pruned %v", pruned)
	}

	// Receipts: categories, horizons and counts, for an audit reader.
	s.expect(s.request("GET", "/v1/operator/retention", "", "", tokenB, ""), 401)
	res := s.request("GET", "/v1/operator/retention", "", "", viewOperator, "")
	s.expect(res, 200)
	var report platform.RetentionReport
	if err = json.Unmarshal(res.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	categories := map[string]platform.RetentionCategory{}
	for _, c := range report.Categories {
		categories[c.Category] = c
	}
	if report.KafkaRetentionDays != 7 || categories["outbox"].MaxAgeDays != 35 || categories["notification_outcomes"].MaxAgeDays != 90 ||
		categories["outbox"].LastPurge == nil || categories["withdrawal_notices"].LastPurge == nil || categories["outbox"].OldestAt == nil {
		t.Fatalf("report %s", res.Body.String())
	}
	if len(report.Purges) < 5 || strings.Contains(res.Body.String(), held) {
		t.Fatalf("receipts %s", res.Body.String())
	}
	// Outside a purge, attempts stay append-only.
	exec(`INSERT INTO notification_withdrawal_attempts (withdrawal_id, attempt, outcome, reason)
	  SELECT id, 1, 'failed', 'rejected' FROM notification_withdrawals WHERE state = 'failed'`)
	if _, err = s.pool.Exec(ctx, `DELETE FROM notification_withdrawal_attempts`); err == nil {
		t.Fatal("an attempt was deleted outside a retention purge")
	}
}
