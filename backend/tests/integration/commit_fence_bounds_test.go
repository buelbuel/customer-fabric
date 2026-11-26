package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/jackc/pgx/v5"
)

// The commit fence is bounded (ADR 0016): a subscription change gives up after a configured wait, every
// writer runs under a deadline the database enforces, a suspension blocks delivery before it waits, and
// one schema's subscription changes never stall another's writes.

// fenceSuite is an activation suite whose API and store share the given fence limits and a monitor.
func fenceSuite(t *testing.T, limits platform.FenceLimits, r *receiver) (activationSuite, *platform.FenceMonitor) {
	t.Helper()
	s := setupActivation(t, r)
	monitor := platform.NewFenceMonitor()
	s.store = platform.Store{Pool: s.pool, Keys: testKeys, Fence: limits, Monitor: monitor}
	handler, err := platform.NewAPI(s.store, s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.h = handler
	return s, monitor
}

// inFlightWrite is a write that has entered the fence and not committed: what a long import chunk, or a
// writer whose client stalled, looks like to a subscription change. Its own deadline is transactionMs.
func inFlightWrite(t *testing.T, s activationSuite, transactionMs int) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err = tx.Exec(ctx, `SELECT enter_commit_fence(60000, $1)`, transactionMs); err != nil {
		t.Fatal(err)
	}
	return tx
}

func (s activationSuite) patch(record, etag, tier string) *httptest.ResponseRecorder {
	return s.request("PATCH", "/v1/profiles/"+record, fmt.Sprintf(`{"set":{"tier":%q}}`, tier), platform.UUID(), tokenA, etag)
}

func (s activationSuite) create(name, purpose string) *httptest.ResponseRecorder {
	return s.request("POST", subscriptionsPath+"/"+name, subscriptionBody("crm_changes", purpose, "console"), "", viewOperator, "")
}

func (s activationSuite) subscriptionCount(name string) int {
	s.t.Helper()
	var n int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM notification_subscriptions WHERE name = $1`, name).Scan(&n); err != nil {
		s.t.Fatal(err)
	}
	return n
}

func within(t *testing.T, what string, c <-chan *httptest.ResponseRecorder, limit time.Duration) *httptest.ResponseRecorder {
	t.Helper()
	select {
	case res := <-c:
		return res
	case <-time.After(limit):
		t.Fatalf("%s did not finish within %s", what, limit)
		return nil
	}
}

// A subscription change behind a long write gives up at its wait with a retriable 503 and creates
// nothing. The writes that queued behind it go on at once, though the write in flight has not committed,
// and the retry succeeds when it has.
func TestSubscriptionChangeGivesUpAndLaterWritesGoOn(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 300 * time.Millisecond, WriteWait: 5 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("bounded_sync", "legitimate_interests")
	record := s.viewRecord(tokenA, "bounded-record", `{"tier":"gold"}`)

	stuck := inFlightWrite(t, s, 60000)
	started := time.Now()
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() { created <- s.create("bounded_sync", purpose) }()
	time.Sleep(100 * time.Millisecond)
	later := make(chan *httptest.ResponseRecorder, 1)
	go func() { later <- s.patch(record, `"1"`, "silver") }()

	res := within(t, "the subscription change", created, 5*time.Second)
	if waited := time.Since(started); res.Code != 503 || res.Header().Get("Retry-After") != "1" || waited < 250*time.Millisecond {
		t.Fatalf("a change behind a long write: %d after %s, %v %s", res.Code, waited, res.Header(), res.Body.String())
	}
	if res = within(t, "the write behind the change", later, 5*time.Second); res.Code != 200 {
		t.Fatalf("the write behind a change that gave up: %d %s", res.Code, res.Body.String())
	}
	if n := s.subscriptionCount("bounded_sync"); n != 0 {
		t.Fatalf("a refused change created %d subscriptions", n)
	}

	s.expect(s.request("GET", "/v1/operator/commit-fence", "", "", tokenB, ""), 401)
	res = s.request("GET", "/v1/operator/commit-fence", "", "", viewOperator, "")
	s.expect(res, 200)
	var report platform.FenceReport
	if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil {
		t.Fatal(err)
	}
	if report.Changes.TimedOut != 1 || report.Changes.Completed != 0 || report.Changes.WaitMsMax < 250 || report.Changes.Blocked != 1 ||
		report.Writes.Blocked < 1 || report.Writes.WaitMsMax < 100 || report.Writes.Waiting != 0 || report.Changes.Waiting != 0 ||
		report.Limits.ChangeWaitMs != 300 || report.Limits.TransactionTimeoutMs != 60000 {
		t.Fatalf("report %+v", report)
	}

	if err := stuck.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res = s.create("bounded_sync", purpose); res.Code != 201 {
		t.Fatalf("the retry: %d %s", res.Code, res.Body.String())
	}
	res = s.request("GET", "/v1/operator/commit-fence", "", "", viewOperator, "")
	if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil || report.Changes.Completed != 1 {
		t.Fatalf("report %+v %v", report, err)
	}
}

// A write whose client stalled after entering the fence is ended by the database at its deadline, with
// what it wrote, so a subscription change that waits for it is not held for ever.
func TestStalledWriteCannotHoldTheFencePastItsDeadline(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 20 * time.Second, WriteWait: 20 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("stalled_sync", "legitimate_interests")
	record := s.viewRecord(tokenA, "stalled-record", `{"tier":"gold"}`)

	stalled := inFlightWrite(t, s, 1000)
	written := platform.UUID()
	if _, err := stalled.Exec(ctx, `INSERT INTO outbox (id, type, profile_id, version, source, payload) VALUES ($1, 'profile.updated.v1', $2, 900, 'console', '{}')`, written, record); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	res := s.create("stalled_sync", purpose)
	if waited := time.Since(started); res.Code != 201 || waited < 700*time.Millisecond || waited > 10*time.Second {
		t.Fatalf("a change behind a stalled write: %d after %s: %s", res.Code, waited, res.Body.String())
	}
	if err := stalled.Commit(ctx); err == nil {
		t.Fatal("a transaction past its deadline committed")
	}
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM outbox WHERE id = $1`, written).Scan(&n); err != nil || n != 0 {
		t.Fatalf("what the ended transaction wrote survived: %d %v", n, err)
	}
}

// Every outbox writer runs under the transaction deadline, whatever it is waiting for: a write stuck on a
// lock is ended at the deadline, long before its lock wait, and the subscription change that queued behind
// it completes then.
func TestWriterStuckOnALockIsEndedByTheDeadline(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 20 * time.Second, WriteWait: 20 * time.Second, TransactionTimeout: time.Second}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("deadline_sync", "legitimate_interests")
	record := s.viewRecord(tokenA, "deadline-record", `{"tier":"gold"}`)

	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	if _, err = blocker.Exec(ctx, `SELECT 1 FROM profiles WHERE id = $1 FOR UPDATE`, record); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	patched := make(chan *httptest.ResponseRecorder, 1)
	go func() { patched <- s.patch(record, `"1"`, "silver") }()
	time.Sleep(300 * time.Millisecond)
	created := make(chan *httptest.ResponseRecorder, 1)
	go func() { created <- s.create("deadline_sync", purpose) }()

	res := within(t, "the stuck write", patched, 10*time.Second)
	if waited := time.Since(started); res.Code != 503 || res.Header().Get("Retry-After") != "1" || waited < 700*time.Millisecond || !strings.Contains(res.Body.String(), "Transaction timed out") {
		t.Fatalf("a writer stuck on a lock: %d after %s: %s", res.Code, waited, res.Body.String())
	}
	if res = within(t, "the change behind it", created, 10*time.Second); res.Code != 201 {
		t.Fatalf("the change behind a stuck writer: %d %s", res.Code, res.Body.String())
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if res = s.patch(record, `"1"`, "silver"); res.Code != 200 {
		t.Fatalf("the retry: %d %s", res.Code, res.Body.String())
	}
}

// A write's lock waits are bounded too: past its wait it is refused as retriable and nothing is written.
func TestWriteWaitIsBounded(t *testing.T) {
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: time.Second, WriteWait: 300 * time.Millisecond, TransactionTimeout: time.Minute}, newReceiver(t))
	ctx := context.Background()
	record := s.viewRecord(tokenA, "wait-record", `{"tier":"gold"}`)
	before := s.count("outbox")
	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	if _, err = blocker.Exec(ctx, `SELECT 1 FROM profiles WHERE id = $1 FOR UPDATE`, record); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	res := s.patch(record, `"1"`, "silver")
	if waited := time.Since(started); res.Code != 503 || res.Header().Get("Retry-After") != "1" || waited < 250*time.Millisecond || waited > 5*time.Second || !strings.Contains(res.Body.String(), "Temporarily busy") {
		t.Fatalf("a write behind a lock: %d after %s: %s", res.Code, waited, res.Body.String())
	}
	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if n := s.count("outbox"); n != before {
		t.Fatalf("a refused write left %d outbox rows", n-before)
	}
	if res = s.patch(record, `"1"`, "silver"); res.Code != 200 {
		t.Fatalf("the retry: %d %s", res.Code, res.Body.String())
	}
	if n := s.count("outbox"); n != before+1 {
		t.Fatalf("outbox rows %d, want %d", n, before+1)
	}
}

// A long import chunk holds the fence: a subscription change gives up meanwhile, the chunk is ended at its
// lock wait with its batch rolled back, and the client resumes at the checkpoint.
func TestLongImportChunkIsBoundedAndResumable(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 300 * time.Millisecond, WriteWait: 1500 * time.Millisecond, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("import_sync", "legitimate_interests")
	imp := s.startImport(tokenA, `{"max_errors":10}`)
	s.expect(s.batch(tokenA, imp.ID, 1, row("imp-1", `{"tier":"gold"}`), row("imp-2", `{"tier":"gold"}`)), 200)

	var profile string
	if err := s.pool.QueryRow(ctx, `SELECT profile_id FROM external_identifiers WHERE value = 'imp-1'`).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	blocker, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Rollback(ctx) })
	if _, err = blocker.Exec(ctx, `SELECT 1 FROM profiles WHERE id = $1 FOR UPDATE`, profile); err != nil {
		t.Fatal(err)
	}
	chunk := make(chan *httptest.ResponseRecorder, 1)
	go func() { chunk <- s.batch(tokenA, imp.ID, 3, row("imp-1", `{"tier":"silver"}`)) }()
	time.Sleep(300 * time.Millisecond)
	if res := s.create("import_sync", purpose); res.Code != 503 {
		t.Fatalf("a change behind an import chunk: %d %s", res.Code, res.Body.String())
	}
	if res := within(t, "the import chunk", chunk, 10*time.Second); res.Code != 503 || res.Header().Get("Retry-After") != "1" {
		t.Fatalf("a chunk stuck on a lock: %d %s", res.Code, res.Body.String())
	}
	res := s.request("GET", "/v1/imports/"+imp.ID, "", "", tokenA, "")
	s.expect(res, 200)
	if got := progress(t, res); got.Checkpoint != 2 {
		t.Fatalf("the ended chunk moved the checkpoint: %+v", got)
	}

	if err = blocker.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if got := progress(t, s.batch(tokenA, imp.ID, 3, row("imp-1", `{"tier":"silver"}`))); got.Checkpoint != 3 {
		t.Fatalf("resumed import %+v", got)
	}
	if res := s.create("import_sync", purpose); res.Code != 201 {
		t.Fatalf("the change after the import: %d %s", res.Code, res.Body.String())
	}
}

// A suspension blocks delivery before it waits for the fence, so a refused suspension has already taken
// effect; repeating it completes it, and resuming cancels it.
func TestSuspensionBlocksDeliveryWhileItWaitsForTheFence(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 300 * time.Millisecond, WriteWait: 5 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("halt_sync", "legitimate_interests")
	subscription := s.subscribe("halt_sync", subscriptionBody("crm_changes", purpose, "console"))
	record := s.viewRecord(tokenA, "halt-record", `{"tier":"gold"}`)
	e := s.outboxEvents(record)[0]
	n := platform.NewNotifier(s.store, s.config.Egress())
	state := func(name, state string) *httptest.ResponseRecorder {
		return s.request("POST", subscriptionsPath+"/"+name+"/state", fmt.Sprintf(`{"state":%q}`, state), "", viewOperator, "")
	}
	listed := func() platform.NotificationSubscription {
		t.Helper()
		res := s.request("GET", subscriptionsPath, "", "", viewOperator, "")
		s.expect(res, 200)
		var list struct {
			Items []platform.NotificationSubscription
		}
		if err := json.Unmarshal(res.Body.Bytes(), &list); err != nil {
			t.Fatal(err)
		}
		for _, item := range list.Items {
			if item.Name == "halt_sync" {
				return item
			}
		}
		t.Fatal("subscription not listed")
		return platform.NotificationSubscription{}
	}

	stuck := inFlightWrite(t, s, 60000)
	// Accepted, not failed: delivery has stopped, and the body says the subscription is active but halted.
	res := state("halt_sync", "suspended")
	var pending platform.NotificationSubscription
	if err := json.Unmarshal(res.Body.Bytes(), &pending); err != nil || res.Code != 202 || res.Header().Get("Retry-After") != "1" ||
		pending.State != "active" || pending.DeliveryHaltedAt == nil {
		t.Fatalf("a suspension behind a long write: %d %s", res.Code, res.Body.String())
	}
	res = s.request("GET", "/v1/operator/commit-fence", "", "", viewOperator, "")
	var report platform.FenceReport
	if err := json.Unmarshal(res.Body.Bytes(), &report); err != nil || report.PendingSuspensions.Count != 1 || report.PendingSuspensions.OldestSince == nil {
		t.Fatalf("a pending suspension is not reported: %s", res.Body.String())
	}
	audited := func(status int) int {
		t.Helper()
		var n int
		if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM operator_audit WHERE resource = $1 AND status = $2`, subscriptionsPath+"/halt_sync/state", status).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if audited(202) != 1 {
		t.Fatalf("a suspension that stopped delivery and was refused is not audited: %d", audited(202))
	}
	if item := listed(); item.State != "active" || item.DeliveryHaltedAt == nil {
		t.Fatalf("a refused suspension is not visible: %+v", item)
	}
	if got, err := n.Notify(ctx, e, subscription); err != nil || got.Outcome != platform.NotificationSuppressed || got.Reason != "subscription_suspended" {
		t.Fatalf("a halted subscription was notified: %+v %v", got, err)
	}
	if len(r.deliveries()) != 0 {
		t.Fatal("something was sent to a subscription whose suspension was requested")
	}

	// Resuming cancels the request: it delivers again.
	if res = state("halt_sync", "active"); res.Code != 200 || strings.Contains(res.Body.String(), "delivery_halted_at") {
		t.Fatalf("resume: %d %s", res.Code, res.Body.String())
	}
	if got, err := n.Notify(ctx, e, subscription); err != nil || got.Outcome != platform.NotificationDelivered {
		t.Fatalf("a resumed subscription: %+v %v", got, err)
	}

	// Requested again, then completed once the write has committed.
	if res = state("halt_sync", "suspended"); res.Code != 202 {
		t.Fatalf("second request: %d %s", res.Code, res.Body.String())
	}
	if audited(202) != 2 {
		t.Fatalf("each request that stopped delivery is audited: %d", audited(202))
	}
	if err := stuck.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res = state("halt_sync", "suspended"); res.Code != 200 || !strings.Contains(res.Body.String(), `"suspended"`) || strings.Contains(res.Body.String(), "delivery_halted_at") {
		t.Fatalf("the repeated suspension: %d %s", res.Code, res.Body.String())
	}
	if audited(200) < 1 {
		t.Fatal("the completed suspension is not audited")
	}
	var halted, epoch bool
	if err := s.pool.QueryRow(ctx, `SELECT delivery_halted_at IS NOT NULL, suspended_epoch IS NOT NULL FROM notification_subscriptions WHERE name = 'halt_sync'`).Scan(&halted, &epoch); err != nil || halted || !epoch {
		t.Fatalf("a completed suspension: halted %v epoch %v %v", halted, epoch, err)
	}
	if res = state("halt_sync", "suspended"); res.Code != 200 {
		t.Fatalf("suspending a suspended subscription: %d", res.Code)
	}
}

// A public suspension waiting on the purpose lock must not overtake a resumption that cleared its halt.
func TestPublicSuspensionDoesNotUndoResumption(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 300 * time.Millisecond, WriteWait: 5 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	purpose, _ := s.purposeWithBasis("public_race", "legitimate_interests")
	s.destination("crm_changes", r)
	s.subscribe("public_race", subscriptionBody("crm_changes", purpose, "console"))
	state := func(state string) *httptest.ResponseRecorder {
		return s.request("POST", subscriptionsPath+"/public_race/state", fmt.Sprintf(`{"state":%q}`, state), "", viewOperator, "")
	}

	stuck := inFlightWrite(t, s, 60000)
	if res := state("suspended"); res.Code != 202 {
		t.Fatalf("the initial suspension: %d %s", res.Code, res.Body.String())
	}
	s.store.Fence.ChangeWait = 15 * time.Second
	handler, err := platform.NewAPI(s.store, s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.h = handler

	policyLock, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer policyLock.Rollback(ctx)
	if _, err = policyLock.Exec(ctx, `LOCK TABLE consent_policy_versions IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	waitingFor := func(fragment string) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var count int
			if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND query LIKE '%' || $1 || '%'`, fragment).Scan(&count); err != nil {
				t.Fatal(err)
			}
			if count > 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("no request waited on %q", fragment)
	}

	resumed := make(chan *httptest.ResponseRecorder, 1)
	go func() { resumed <- state("active") }()
	waitingFor("consent_policy_versions")
	suspended := make(chan *httptest.ResponseRecorder, 1)
	go func() { suspended <- state("suspended") }()
	waitingFor("consent-purpose:")
	if err = policyLock.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res := within(t, "the resumption", resumed, 5*time.Second); res.Code != 200 {
		t.Fatalf("the resumption: %d %s", res.Code, res.Body.String())
	}

	var result *httptest.ResponseRecorder
	select {
	case result = <-suspended:
	case <-time.After(100 * time.Millisecond):
	}
	if err = stuck.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if result == nil {
		result = within(t, "the later suspension", suspended, 5*time.Second)
	}
	var subscription platform.NotificationSubscription
	if err = json.Unmarshal(result.Body.Bytes(), &subscription); err != nil ||
		result.Code != 200 || subscription.State != "active" || subscription.DeliveryHaltedAt != nil {
		t.Fatalf("a suspension overtook the resumption: %d %s (%v)", result.Code, result.Body.String(), err)
	}
}

// Advisory locks are database-wide, so the fence is keyed by schema: a subscription change waiting in
// one schema stalls neither the writes nor the subscription changes of another in the same database.
func TestSubscriptionChangeInOneSchemaDoesNotStallAnother(t *testing.T) {
	r := newReceiver(t)
	limits := platform.FenceLimits{ChangeWait: 30 * time.Second, WriteWait: 30 * time.Second, TransactionTimeout: time.Minute}
	a, _ := fenceSuite(t, limits, r)
	b, _ := fenceSuite(t, limits, r)
	ctx := context.Background()
	for _, s := range []activationSuite{a, b} {
		s.destination("crm_changes", r)
	}
	purposeA, _ := a.purposeWithBasis("iso_sync", "legitimate_interests")
	purposeB, _ := b.purposeWithBasis("iso_sync", "legitimate_interests")
	recordB := b.viewRecord(tokenA, "iso-record", `{"tier":"gold"}`)

	stuck := inFlightWrite(t, a, 60000)
	pending := make(chan *httptest.ResponseRecorder, 1)
	go func() { pending <- a.create("iso_a", purposeA) }()
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	if res := b.patch(recordB, `"1"`, "silver"); res.Code != 200 {
		t.Fatalf("a write in another schema: %d %s", res.Code, res.Body.String())
	}
	if res := b.create("iso_b", purposeB); res.Code != 201 {
		t.Fatalf("a change in another schema: %d %s", res.Code, res.Body.String())
	}
	if waited := time.Since(started); waited > 2*time.Second {
		t.Fatalf("another schema's requests waited %s", waited)
	}
	select {
	case res := <-pending:
		t.Fatalf("the change did not wait for its own schema's write: %d", res.Code)
	default:
	}
	if err := stuck.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if res := within(t, "the change in its own schema", pending, 10*time.Second); res.Code != 201 {
		t.Fatalf("%d %s", res.Code, res.Body.String())
	}
}

// Bounds are required, not optional.
func TestTransactionsNeedFiniteBounds(t *testing.T) {
	s := setup(t)
	ctx := context.Background()
	for _, call := range []string{`SELECT bound_transaction(0, 1000)`, `SELECT bound_transaction(1000, 0)`, `SELECT enter_commit_fence(NULL, 1000)`, `SELECT enter_commit_fence(1000, -1)`} {
		if _, err := s.pool.Exec(ctx, call); err == nil || !strings.Contains(err.Error(), "finite bounds") {
			t.Fatalf("%s: %v", call, err)
		}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	var lock, deadline string
	if _, err = tx.Exec(ctx, `SELECT enter_commit_fence(1500, 2000)`); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, `SELECT current_setting('lock_timeout'), current_setting('transaction_timeout')`).Scan(&lock, &deadline); err != nil || lock != "1500ms" || deadline != "2s" {
		t.Fatalf("bounds %q %q %v", lock, deadline, err)
	}
}

// Commit order stays exact under contention: writers, subscription creations that give up and retry, and
// writes that overstay the change wait. A write that finished before a subscription's creation began has
// an earlier epoch, one that began after it finished a later or equal one, and one record's changes never
// go back in epoch.
func TestCommitOrderHoldsUnderConcurrentChangesAndWrites(t *testing.T) {
	r := newReceiver(t)
	s, monitor := fenceSuite(t, platform.FenceLimits{ChangeWait: 250 * time.Millisecond, WriteWait: 20 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("order_sync", "legitimate_interests")

	type window struct{ start, end time.Time }
	type write struct {
		record  string
		version int64
		window
	}
	const writers, perWriter, subscriptions = 4, 25, 10
	records := make([]string, writers)
	for i := range records {
		records[i] = s.viewRecord(tokenA, fmt.Sprintf("order-record-%d", i), `{"tier":"gold"}`)
	}

	var mu sync.Mutex
	var writes []write
	created := map[string]window{}
	var group sync.WaitGroup
	stop := make(chan struct{})
	for i := range writers {
		group.Add(1)
		go func() {
			defer group.Done()
			etag := `"1"`
			for n := range perWriter {
				for {
					started := time.Now()
					res := s.patch(records[i], etag, fmt.Sprintf("tier-%d", n))
					if res.Code == 503 {
						continue
					}
					if res.Code != 200 {
						t.Errorf("write: %d %s", res.Code, res.Body.String())
						return
					}
					var receipt struct{ Version int64 }
					if err := json.Unmarshal(res.Body.Bytes(), &receipt); err != nil {
						t.Error(err)
						return
					}
					etag = res.Header().Get("ETag")
					mu.Lock()
					writes = append(writes, write{records[i], receipt.Version, window{started, time.Now()}})
					mu.Unlock()
					break
				}
			}
		}()
	}
	// Writes that overstay the change wait, so creations are refused and retried.
	holders := make(chan struct{})
	go func() {
		defer close(holders)
		for {
			select {
			case <-stop:
				return
			default:
			}
			tx, err := s.pool.Begin(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			if _, err = tx.Exec(ctx, `SELECT enter_commit_fence(60000, 60000)`); err != nil {
				t.Error(err)
				return
			}
			time.Sleep(400 * time.Millisecond)
			_ = tx.Commit(ctx)
			time.Sleep(60 * time.Millisecond)
		}
	}()
	var makers sync.WaitGroup
	for w := range 2 {
		makers.Add(1)
		go func() {
			defer makers.Done()
			for i := w; i < subscriptions; i += 2 {
				name := fmt.Sprintf("order_%d", i)
				for attempt := 0; ; attempt++ {
					if attempt > 200 {
						t.Errorf("%s was never created", name)
						return
					}
					started := time.Now()
					res := s.create(name, purpose)
					if res.Code == 201 {
						mu.Lock()
						created[name] = window{started, time.Now()}
						mu.Unlock()
						break
					}
					if res.Code != 503 {
						t.Errorf("%s: %d %s", name, res.Code, res.Body.String())
						return
					}
					time.Sleep(20 * time.Millisecond)
				}
			}
		}()
	}
	group.Wait()
	makers.Wait()
	close(stop)
	<-holders
	if t.Failed() {
		return
	}

	epochs := map[string]int64{}
	rows, err := s.pool.Query(ctx, `SELECT profile_id::text || ':' || version, commit_epoch FROM outbox WHERE profile_id = ANY($1)`, records)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var key string
		var epoch int64
		if err = rows.Scan(&key, &epoch); err != nil {
			t.Fatal(err)
		}
		epochs[key] = epoch
	}
	rows.Close()
	subscriptionEpochs := map[string]int64{}
	rows, err = s.pool.Query(ctx, `SELECT name, created_epoch FROM notification_subscriptions WHERE name LIKE 'order\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name string
		var epoch int64
		if err = rows.Scan(&name, &epoch); err != nil {
			t.Fatal(err)
		}
		subscriptionEpochs[name] = epoch
	}
	rows.Close()
	if len(writes) != writers*perWriter || len(subscriptionEpochs) != subscriptions {
		t.Fatalf("%d writes, %d subscriptions", len(writes), len(subscriptionEpochs))
	}
	checked := 0
	for _, w := range writes {
		epoch, ok := epochs[fmt.Sprintf("%s:%d", w.record, w.version)]
		if !ok {
			t.Fatalf("no outbox row for %s version %d", w.record, w.version)
		}
		for name, c := range created {
			switch {
			case w.end.Before(c.start):
				checked++
				if epoch >= subscriptionEpochs[name] {
					t.Fatalf("a write that finished before %s began carries epoch %d, the subscription %d", name, epoch, subscriptionEpochs[name])
				}
			case w.start.After(c.end):
				checked++
				if epoch < subscriptionEpochs[name] {
					t.Fatalf("a write that began after %s finished carries epoch %d, the subscription %d", name, epoch, subscriptionEpochs[name])
				}
			}
		}
	}
	if checked < writers*perWriter {
		t.Fatalf("only %d ordered pairs were checked; the run did not interleave", checked)
	}
	for _, record := range records {
		last := int64(-1)
		for version := int64(1); version <= perWriter+1; version++ {
			epoch, ok := epochs[fmt.Sprintf("%s:%d", record, version)]
			if !ok || epoch < last {
				t.Fatalf("%s version %d: epoch %d after %d (%v)", record, version, epoch, last, ok)
			}
			last = epoch
		}
	}
	report := monitor.Report(platform.FenceLimits{})
	if report.Changes.TimedOut < 1 || report.Changes.Completed != subscriptions || report.Writes.Completed < writers*perWriter ||
		report.Writes.Waiting != 0 || report.Changes.Waiting != 0 {
		t.Fatalf("report %+v", report)
	}
}

// A suspension that stopped delivery and did not complete does not stay pending: the housekeeper completes
// it once the writes in flight have committed, never one that was resumed meanwhile, and the report counts
// what is pending.
func TestPendingSuspensionsAreCompleted(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: 300 * time.Millisecond, WriteWait: 5 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purpose, _ := s.purposeWithBasis("pending_sync", "legitimate_interests")
	s.subscribe("pending_a", subscriptionBody("crm_changes", purpose, "console"))
	s.subscribe("pending_b", subscriptionBody("crm_changes", purpose, "console"))
	state := func(name, state string) *httptest.ResponseRecorder {
		return s.request("POST", subscriptionsPath+"/"+name+"/state", fmt.Sprintf(`{"state":%q}`, state), "", viewOperator, "")
	}

	stuck := inFlightWrite(t, s, 60000)
	s.expect(state("pending_a", "suspended"), 202)
	s.expect(state("pending_b", "suspended"), 202)
	if got, err := s.store.PendingSuspensions(ctx); err != nil || got.Count != 2 || got.OldestSince == nil {
		t.Fatalf("pending %+v %v", got, err)
	}
	// Within its grace a suspension is left to its requester.
	if n, err := s.store.CompletePendingSuspensions(ctx, time.Hour); err != nil || n != 0 {
		t.Fatalf("completed within the grace: %d %v", n, err)
	}
	// While the write is in flight it cannot complete, and stays blocked and pending.
	if n, err := s.store.CompletePendingSuspensions(ctx, 0); err != nil || n != 0 {
		t.Fatalf("completed behind a write in flight: %d %v", n, err)
	}
	if got, _ := s.store.PendingSuspensions(ctx); got.Count != 2 {
		t.Fatalf("pending %+v", got)
	}
	// A resumption that comes meanwhile is the operator's later decision, and is kept.
	s.expect(state("pending_b", "active"), 200)
	if err := stuck.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if n, err := s.store.CompletePendingSuspensions(ctx, 0); err != nil || n != 1 {
		t.Fatalf("completed %d %v", n, err)
	}
	if n, err := s.store.CompletePendingSuspensions(ctx, 0); err != nil || n != 0 {
		t.Fatalf("completed twice: %d %v", n, err)
	}
	states := map[string]string{}
	rows, err := s.pool.Query(ctx, `SELECT name, state FROM notification_subscriptions WHERE name LIKE 'pending\_%'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var name, st string
		if err = rows.Scan(&name, &st); err != nil {
			t.Fatal(err)
		}
		states[name] = st
	}
	rows.Close()
	if states["pending_a"] != "suspended" || states["pending_b"] != "active" {
		t.Fatalf("states %v", states)
	}
	if got, err := s.store.PendingSuspensions(ctx); err != nil || got.Count != 0 || got.OldestSince != nil {
		t.Fatalf("pending %+v %v", got, err)
	}
	// The completion is the same suspension: its order against changes is by commit, and it is idempotent.
	var epoch bool
	if err = s.pool.QueryRow(ctx, `SELECT suspended_epoch IS NOT NULL FROM notification_subscriptions WHERE name = 'pending_a'`).Scan(&epoch); err != nil || !epoch {
		t.Fatalf("epoch %v %v", epoch, err)
	}
}

// A resumption that comes while the housekeeper works through its list is not undone: whether a
// suspension is still pending is decided under the subscription's purpose lock, when its turn comes.
func TestResumptionDuringCompletionIsKept(t *testing.T) {
	r := newReceiver(t)
	s, _ := fenceSuite(t, platform.FenceLimits{ChangeWait: time.Second, WriteWait: 5 * time.Second, TransactionTimeout: time.Minute}, r)
	ctx := context.Background()
	s.destination("crm_changes", r)
	purposeA, _ := s.purposeWithBasis("race_a", "legitimate_interests")
	purposeB, _ := s.purposeWithBasis("race_b", "legitimate_interests")
	s.subscribe("race_a", subscriptionBody("crm_changes", purposeA, "console"))
	s.subscribe("race_b", subscriptionBody("crm_changes", purposeB, "console"))
	state := func(name, state string) *httptest.ResponseRecorder {
		return s.request("POST", subscriptionsPath+"/"+name+"/state", fmt.Sprintf(`{"state":%q}`, state), "", viewOperator, "")
	}

	stuck := inFlightWrite(t, s, 60000)
	s.expect(state("race_a", "suspended"), 202)
	time.Sleep(20 * time.Millisecond)
	s.expect(state("race_b", "suspended"), 202)

	// The housekeeper listed both, and waits on race_a for the write in flight.
	completed := make(chan int, 1)
	go func() {
		n, err := s.store.CompletePendingSuspensions(ctx, 0)
		if err != nil {
			t.Error(err)
		}
		completed <- n
	}()
	time.Sleep(300 * time.Millisecond)
	s.expect(state("race_b", "active"), 200)
	if n := <-completed; n != 0 {
		t.Fatalf("completed %d while the write was in flight", n)
	}
	if err := stuck.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	// Both were listed together in the run above; this one finds race_a only.
	if n, err := s.store.CompletePendingSuspensions(ctx, 0); err != nil || n != 1 {
		t.Fatalf("completed %d %v", n, err)
	}
	var a, b string
	if err := s.pool.QueryRow(ctx, `SELECT (SELECT state FROM notification_subscriptions WHERE name = 'race_a'), (SELECT state FROM notification_subscriptions WHERE name = 'race_b')`).Scan(&a, &b); err != nil || a != "suspended" || b != "active" {
		t.Fatalf("race_a %s, race_b %s: %v", a, b, err)
	}
}
