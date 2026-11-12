package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/workflows"
	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"
)

const policyVersionsPath = "/v1/operator/consent/policy-versions/"

// policyImpact reads what retiring or activating a policy version does to subscriptions.
func (s linkingSuite) policyImpact(version string) platform.PolicyImpact {
	s.t.Helper()
	res := s.request("GET", policyVersionsPath+version+"/impact", "", "", viewOperator, "")
	s.expect(res, 200)
	var impact platform.PolicyImpact
	if err := json.Unmarshal(res.Body.Bytes(), &impact); err != nil {
		s.t.Fatal(err)
	}
	return impact
}

// retirePolicy retires a policy version, acknowledging the subscriptions it interrupts.
func (s linkingSuite) retirePolicy(version string) {
	s.t.Helper()
	body := fmt.Sprintf(`{"state":"retired","acknowledged_impact":%q}`, s.policyImpact(version).Acknowledgment)
	s.expect(s.request("POST", policyVersionsPath+version+"/state", body, "", viewOperator, ""), 200)
}

// activateNextPolicy drafts and activates a further legitimate-interests version of a purpose.
func (s linkingSuite) activateNextPolicy(purpose string) string {
	s.t.Helper()
	res := s.request("POST", "/v1/operator/consent/policies/"+purpose, `{"explanation":"Synthetic explanation.","lawful_basis":"legitimate_interests","notice_reference":"n2","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`, "", viewOperator, "")
	s.expect(res, 201)
	var next struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &next)
	s.expect(s.request("POST", policyVersionsPath+next.ID+"/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	return next.ID
}

// outboxEvents rebuilds a record's committed changes as the relay publishes them, oldest first.
func (s linkingSuite) outboxEvents(profile string) []events.Event {
	s.t.Helper()
	rows, err := s.pool.Query(context.Background(), `SELECT id, type, source, version, occurred_at FROM outbox WHERE profile_id = $1 ORDER BY version`, profile)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	var out []events.Event
	for rows.Next() {
		e := events.Event{Schema: events.Schema, Tenant: "acme", ProfileID: profile}
		if err := rows.Scan(&e.ID, &e.Type, &e.Source, &e.ProfileVersion, &e.OccurredAt); err != nil {
			s.t.Fatal(err)
		}
		e.OccurredAt = e.OccurredAt.UTC()
		out = append(out, e)
	}
	return out
}

// syncWorkflows stands in for Temporal: it runs RecoverNotification's steps at once, and refuses a
// workflow ID it already started, as Temporal does.
type syncWorkflows struct {
	notifier platform.Notifier
	started  map[string]bool
}

func (f *syncWorkflows) ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, _ any, args ...any) (client.WorkflowRun, error) {
	if f.started[options.ID] {
		return nil, serviceerror.NewWorkflowExecutionAlreadyStarted("started", "", "")
	}
	f.started[options.ID] = true
	e, subscription := args[0].(events.Event), args[1].(string)
	result, err := f.notifier.Notify(ctx, e, subscription)
	switch {
	case errors.Is(err, platform.ErrNotificationRejected), errors.Is(err, platform.ErrNotificationUnavailable):
		result = platform.NotificationResult{Outcome: platform.NotificationFailed, Reason: "rejected", StatusCode: result.StatusCode}
	case err != nil:
		return nil, err
	}
	result.Attempts = 1
	return nil, f.notifier.Record(ctx, e, subscription, result)
}

// crashOnce fails the first time an item is marked started, as a process that dies right after starting
// a workflow would.
type crashOnce struct {
	platform.Store
	crashed bool
}

func (c *crashOnce) MarkRecoveryStarted(ctx context.Context, subscription, eventID string) error {
	if !c.crashed {
		c.crashed = true
		return errors.New("synthetic crash")
	}
	return c.Store.MarkRecoveryStarted(ctx, subscription, eventID)
}

// A policy change interrupts the subscriptions approved under it only after an operator acknowledged
// exactly which; while interrupted, their changes are recorded as suppressed. Neither the new version nor
// the reapproval sends anything missed. An explicit recovery does, once per event, newest change per
// record only, and each authorized again from current state: an intervening objection or restriction
// still withholds it.
func TestPolicyChangeInterruptsAndRecoveryResendsSafely(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	ctx := context.Background()
	store := s.store
	n := platform.NewNotifier(store, s.config.Egress())
	s.destination("crm_changes", r)
	purpose, policy := s.purposeWithBasis("recover_sync", "legitimate_interests")
	old := s.subscribe("recover_sync", subscriptionBody("crm_changes", purpose, "console"))
	// notifyAll runs a change through every subscription it would reach, as the profile-change workflow does.
	notifyAll := func(e events.Event) map[string]string {
		t.Helper()
		subscriptions, err := n.Subscriptions(ctx, e)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, subscription := range subscriptions {
			result, err := n.Notify(ctx, e, subscription)
			if err != nil {
				t.Fatal(err)
			}
			if err = n.Record(ctx, e, subscription, result); err != nil {
				t.Fatal(err)
			}
			out[subscription] = result.Outcome + ":" + result.Reason
		}
		return out
	}
	latest := func(profile string) events.Event {
		all := s.outboxEvents(profile)
		return all[len(all)-1]
	}
	update := func(profile string) events.Event {
		t.Helper()
		var version int64
		if err := s.pool.QueryRow(ctx, `SELECT version FROM profiles WHERE id = $1`, profile).Scan(&version); err != nil {
			t.Fatal(err)
		}
		s.expect(s.request("PATCH", "/v1/profiles/"+profile, `{"set":{"tier":"silver`+fmt.Sprint(version)+`"}}`, platform.UUID(), tokenA, fmt.Sprintf(`"%d"`, version)), 200)
		return latest(profile)
	}

	// Before the change: delivered.
	ada := s.viewRecord(tokenA, "recover-ada", `{"tier":"gold"}`)
	if got := notifyAll(latest(ada))[old]; got != "delivered:accepted" {
		t.Fatalf("before the policy change: %s", got)
	}

	// The impact names the subscriptions retirement would interrupt. Retiring needs that acknowledgment,
	// and one made stale by a concurrent change to the subscriptions is refused.
	impact := s.policyImpact(policy)
	if impact.Transition != "retire" || len(impact.Subscriptions) != 1 || impact.Subscriptions[0].Name != "recover_sync" ||
		impact.Subscriptions[0].Effect != "interrupted" || impact.Acknowledgment == "" {
		t.Fatalf("impact: %+v", impact)
	}
	retire := policyVersionsPath + policy + "/state"
	s.expect(s.request("POST", retire, `{"state":"retired"}`, "", viewOperator, ""), 409)
	s.expect(s.request("POST", retire, `{"state":"retired","acknowledged_impact":"not-an-acknowledgment"}`, "", viewOperator, ""), 422)
	s.subscribe("recover_extra", subscriptionBody("crm_changes", purpose, "console"))
	s.expect(s.request("POST", retire, fmt.Sprintf(`{"state":"retired","acknowledged_impact":%q}`, impact.Acknowledgment), "", viewOperator, ""), 409)
	s.expect(s.request("GET", policyVersionsPath+policy+"/impact", "", "", tokenA, ""), 401)
	if names := s.policyImpact(policy).Subscriptions; len(names) != 2 {
		t.Fatalf("a refused retirement interrupted something: %+v", names)
	}
	s.retirePolicy(policy)

	// Interrupted: visible, audited, and its changes are suppressed, not dropped.
	listed := s.request("GET", subscriptionsPath, "", "", viewOperator, "")
	s.expect(listed, 200)
	var list struct {
		Items []platform.NotificationSubscription
	}
	_ = json.Unmarshal(listed.Body.Bytes(), &list)
	for _, item := range list.Items {
		if item.Name == "recover_sync" && (item.State != "needs_reapproval" || item.Interruption == nil || item.Interruption.Until != nil) {
			t.Fatalf("interrupted subscription: %+v", item)
		}
	}
	audit := s.request("GET", subscriptionsPath+"/recover_sync/audit", "", "", viewOperator, "")
	if audit.Code != 200 || !strings.Contains(audit.Body.String(), `"action":"interrupted"`) || !strings.Contains(audit.Body.String(), policy) {
		t.Fatalf("audit: %d %s", audit.Code, audit.Body.String())
	}
	s.expect(s.request("POST", subscriptionsPath+"/recover_sync/state", `{"state":"active"}`, "", viewOperator, ""), 409)
	adaV2 := update(ada)
	adaV3 := update(ada)
	bob := s.viewRecord(tokenA, "recover-bob", `{"tier":"gold"}`)
	cy := s.viewRecord(tokenA, "recover-cy", `{"tier":"gold"}`)
	dan := s.viewRecord(tokenA, "recover-dan", `{"tier":"gold"}`)
	for _, e := range []events.Event{adaV2, adaV3, latest(bob), latest(cy), latest(dan)} {
		if got := notifyAll(e)[old]; got != "suppressed:subscription_needs_reapproval" {
			t.Fatalf("during the interruption: %s", got)
		}
	}
	// gus changes now, but its event is processed only after the reapproval, as by a lagging consumer.
	gus := s.viewRecord(tokenA, "recover-gus", `{"tier":"gold"}`)

	// A new version restores nothing, and cannot be acknowledged as if it interrupted anything.
	res := s.request("POST", "/v1/operator/consent/policies/"+purpose, `{"explanation":"Synthetic explanation.","lawful_basis":"legitimate_interests","notice_reference":"n2","jurisdiction":"DE","locale":"de-DE","channel":"email","retention_reference":"r"}`, "", viewOperator, "")
	s.expect(res, 201)
	var next struct{ ID string }
	_ = json.Unmarshal(res.Body.Bytes(), &next)
	if draft := s.policyImpact(next.ID); draft.Transition != "activate" || len(draft.Subscriptions) != 2 || draft.Subscriptions[0].Effect != "remains_interrupted" || draft.Acknowledgment != "" {
		t.Fatalf("activation impact: %+v", draft)
	}
	s.expect(s.request("POST", policyVersionsPath+next.ID+"/state", `{"state":"active","acknowledged_impact":"stale"}`, "", viewOperator, ""), 409)
	s.expect(s.request("POST", policyVersionsPath+next.ID+"/state", `{"state":"active"}`, "", viewOperator, ""), 200)
	eve := s.viewRecord(tokenA, "recover-eve", `{"tier":"gold"}`)
	if got := notifyAll(latest(eve))[old]; got != "suppressed:subscription_needs_reapproval" {
		t.Fatalf("after the new version: %s", got)
	}

	// Recovery needs a reapproved subscription; reapproval needs an interrupted one and its assessment.
	recoveries := subscriptionsPath + "/recover_sync_v2/recoveries"
	s.expect(s.request("POST", subscriptionsPath+"/recover_sync/recoveries", "", "recover-sync-old-0001", viewOperator, ""), 409)
	s.expect(s.request("POST", subscriptionsPath+"/recover_sync/reapprove", `{"name":"recover_sync_v2"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", subscriptionsPath+"/recover_sync/reapprove", `{"name":"recover_sync_v2","basis_assessment":"LIA-2026-018"}`, "", tokenA, ""), 401)
	delivered := len(r.deliveries())
	reapproved := s.request("POST", subscriptionsPath+"/recover_sync/reapprove", `{"name":"recover_sync_v2","basis_assessment":"LIA-2026-018"}`, "", viewOperator, "")
	s.expect(reapproved, 201)
	if !strings.Contains(reapproved.Body.String(), `"replaces":"recover_sync"`) || !strings.Contains(reapproved.Body.String(), next.ID) {
		t.Fatalf("reapproval: %s", reapproved.Body.String())
	}
	s.expect(s.request("POST", subscriptionsPath+"/recover_sync/reapprove", `{"name":"recover_sync_v3","basis_assessment":"LIA-2026-018"}`, "", viewOperator, ""), 409)
	if len(r.deliveries()) != delivered {
		t.Fatal("reapproval sent the backlog")
	}
	var v2 string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM notification_subscriptions WHERE name = 'recover_sync_v2'`).Scan(&v2); err != nil {
		t.Fatal(err)
	}
	if tried, err := n.Subscriptions(ctx, latest(eve)); err != nil || slices.Contains(tried, v2) {
		t.Fatalf("a change from the interruption was offered to the reapproved subscription: %v %v", tried, err)
	}
	// The late event goes where its commit belonged, the interrupted subscription, never to the new one
	// on its own: it is suppressed there and recoverable.
	late := notifyAll(latest(gus))
	if late[old] != "suppressed:subscription_superseded" || late[v2] != "" {
		t.Fatalf("a change from the interruption processed after reapproval: %v", late)
	}

	// After reapproval: a newer change of bob is delivered at once; cy is objected to and dan restricted
	// before recovery. bob's earlier suppression is past the recovery window.
	bobV2 := update(bob)
	if got := notifyAll(bobV2)[v2]; got != "delivered:accepted" {
		t.Fatalf("after reapproval: %s", got)
	}
	s.recordEvidence(cy, next.ID, "object")
	nextMonth := time.Now().UTC().AddDate(0, 1, 0).Format(time.DateOnly)
	s.expect(s.request("POST", restrictionPath(dan), restrictBody("accuracy_contested", nextMonth), "", viewOperator, ""), 200)
	if _, err := s.pool.Exec(ctx, `UPDATE notification_deliveries SET recorded_at = now() - interval '40 days' WHERE subscription_id = $1 AND profile_id = $2`, old, bob); err != nil {
		t.Fatal(err)
	}

	// recover_extra is abandoned: suspended, it stops recording what it misses.
	s.expect(s.request("POST", subscriptionsPath+"/recover_extra/state", `{"state":"suspended"}`, "", viewOperator, ""), 200)
	var extra string
	if err := s.pool.QueryRow(ctx, `SELECT id FROM notification_subscriptions WHERE name = 'recover_extra'`).Scan(&extra); err != nil {
		t.Fatal(err)
	}
	afterAbandoning := update(eve)
	if tried, _ := n.Subscriptions(ctx, afterAbandoning); slices.Contains(tried, extra) {
		t.Fatal("an abandoned subscription still records what it misses")
	}

	// The recovery: authorized, idempotent per key, each event once across requests.
	s.expect(s.request("POST", recoveries, "", "recover-sync-key-0001", tokenA, ""), 401)
	s.expect(s.request("POST", recoveries, "", "", viewOperator, ""), 400)
	first := s.request("POST", recoveries, "", "recover-sync-key-0001", viewOperator, "")
	s.expect(first, 201)
	var recovery platform.NotificationRecovery
	_ = json.Unmarshal(first.Body.Bytes(), &recovery)
	// eve v1, gus, ada v3, cy and dan are queued; ada v2 is superseded by v3; bob v1 is past the window.
	want := map[string]int64{"queued": 5, "started": 0, "superseded": 1, "unrecoverable": 1}
	if fmt.Sprint(recovery.Items) != fmt.Sprint(want) || recovery.MorePending {
		t.Fatalf("recovery: %+v", recovery)
	}
	replay := s.request("POST", recoveries, "", "recover-sync-key-0001", viewOperator, "")
	s.expect(replay, 200)
	if !strings.Contains(replay.Body.String(), recovery.ID) {
		t.Fatalf("replay: %s", replay.Body.String())
	}
	second := s.request("POST", recoveries, "", "recover-sync-key-0002", viewOperator, "")
	s.expect(second, 201)
	if !strings.Contains(second.Body.String(), `"queued":0`) || !strings.Contains(second.Body.String(), `"superseded":0`) {
		t.Fatalf("a second request took events again: %s", second.Body.String())
	}
	if len(r.deliveries()) != delivered+1 {
		t.Fatal("requesting a recovery sent something by itself")
	}
	// ada changes again and v4 is delivered live before the recovery runs: queued v3 is then withheld.
	if got := notifyAll(update(ada))[v2]; got != "delivered:accepted" {
		t.Fatalf("ada v4: %s", got)
	}

	// The starter crashes after starting one workflow; started again, it neither loses nor repeats it.
	temporal := &syncWorkflows{notifier: n, started: map[string]bool{}}
	crashing := &crashOnce{Store: store}
	if _, err := (workflows.RecoveryStarter{Store: crashing, Temporal: temporal, Tenant: "acme"}).StartOnce(ctx); err == nil {
		t.Fatal("the crash was not reported")
	}
	for {
		started, err := (workflows.RecoveryStarter{Store: crashing, Temporal: temporal, Tenant: "acme"}).StartOnce(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if started == 0 {
			break
		}
	}
	got := s.request("GET", recoveries+"/"+recovery.ID, "", "", viewOperator, "")
	s.expect(got, 200)
	_ = json.Unmarshal(got.Body.Bytes(), &recovery)
	if recovery.Items["started"] != 5 || recovery.Outcomes["delivered"] != 2 || recovery.Outcomes["suppressed"] != 3 {
		t.Fatalf("recovered: %+v", recovery)
	}
	sent := r.deliveries()[delivered+2:]
	keys := make([]string, len(sent))
	for i, d := range sent {
		keys[i] = d.key
	}
	slices.Sort(keys)
	wantKeys := []string{s.outboxEvents(eve)[0].ID, latest(gus).ID}
	slices.Sort(wantKeys)
	if !slices.Equal(keys, wantKeys) {
		t.Fatalf("recovery sent %v, want eve v1 and gus once each", keys)
	}

	// Each item says what happened to it, page by page.
	page := s.request("GET", recoveries+"/"+recovery.ID+"/items?limit=4", "", "", viewOperator, "")
	s.expect(page, 200)
	var items struct {
		Items []platform.RecoveryItem
		Next  string
	}
	_ = json.Unmarshal(page.Body.Bytes(), &items)
	reasons := map[string]string{}
	for _, item := range items.Items {
		reasons[item.ProfileID+fmt.Sprint(item.ProfileVersion)] = item.State + ":" + item.Reason + item.OutcomeReason
	}
	rest := s.request("GET", recoveries+"/"+recovery.ID+"/items?after="+items.Next, "", "", viewOperator, "")
	s.expect(rest, 200)
	items.Items, items.Next = nil, ""
	_ = json.Unmarshal(rest.Body.Bytes(), &items)
	for _, item := range items.Items {
		reasons[item.ProfileID+fmt.Sprint(item.ProfileVersion)] = item.State + ":" + item.Reason + item.OutcomeReason
	}
	for key, want := range map[string]string{ada + "2": "superseded:newer_change_notified", ada + "3": "started:newer_change_notified", gus + "1": "started:accepted", bob + "1": "unrecoverable:outside_recovery_window",
		cy + "1": "started:purpose_not_permitted", dan + "1": "started:record_restricted", eve + "1": "started:accepted"} {
		if reasons[key] != want {
			t.Errorf("item %s: %q, want %q", key, reasons[key], want)
		}
	}
	s.expect(s.request("GET", recoveries+"/"+recovery.ID+"/items?limit=0", "", "", viewOperator, ""), 400)
	s.expect(s.request("GET", recoveries+"/"+platform.UUID(), "", "", viewOperator, ""), 404)

	// An abandoned subscription can still be reapproved later, as a new subscription.
	s.expect(s.request("POST", subscriptionsPath+"/recover_extra/reapprove", `{"name":"recover_extra_v2","basis_assessment":"LIA-2026-018"}`, "", viewOperator, ""), 201)
	// A change made after it was abandoned does not become recoverable through it, however late it is processed.
	if tried, _ := n.Subscriptions(ctx, afterAbandoning); slices.Contains(tried, extra) {
		t.Fatal("a change made after abandoning was offered to the abandoned subscription")
	}

	// The new subscription's audit trail names the reapproval and both recovery requests.
	trail := s.request("GET", subscriptionsPath+"/recover_sync_v2/audit", "", "", viewOperator, "")
	if body := trail.Body.String(); strings.Count(body, `"action":"recovery_requested"`) != 2 || !strings.Contains(body, `"action":"reapproved"`) {
		t.Fatalf("audit: %s", body)
	}
}
