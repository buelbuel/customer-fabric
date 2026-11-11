package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// interruptionReasons are the suppression reasons that mean a subscription's policy version no longer
// authorized it, including a change committed during the interruption but processed only after the
// subscription was superseded. Only notifications suppressed for one of these can be recovered (ADR 0016).
var interruptionReasons = []string{"subscription_needs_reapproval", "subscription_superseded", "basis_not_approved", "no_lawful_basis"}

const (
	// recoveryBatch bounds one recovery request; a later request with a new key takes the rest.
	recoveryBatch = 10000
	// recoveryWindow is how long a suppressed notification stays recoverable.
	recoveryWindow = 30 * 24 * time.Hour
)

// PolicyImpact says what a policy version's next transition does to notification subscriptions.
type PolicyImpact struct {
	PolicyVersionID string `json:"policy_version_id"`
	// Transition is retire for the active version and activate for a draft.
	Transition    string                 `json:"transition"`
	Subscriptions []ImpactedSubscription `json:"subscriptions"`
	// Acknowledgment must be sent with the transition when it interrupts subscriptions. It names exactly
	// the subscriptions listed, so a change to them since makes it stale.
	Acknowledgment string `json:"acknowledgment,omitempty"`
}

// ImpactedSubscription is one subscription a transition affects, and how.
type ImpactedSubscription struct {
	Name          string   `json:"name"`
	Destination   string   `json:"destination"`
	RecipientRole string   `json:"recipient_role"`
	EventTypes    []string `json:"event_types"`
	Sources       []string `json:"sources"`
	// Effect is interrupted (it stops authorizing notifications until reapproved) or
	// remains_interrupted (activation does not restore it; it needs reapproval).
	Effect string `json:"effect"`
}

func impacted(rows []db.NotificationSubscription, effect string) []ImpactedSubscription {
	out := make([]ImpactedSubscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, ImpactedSubscription{Name: r.Name, Destination: r.Destination, RecipientRole: r.RecipientRole,
			EventTypes: r.EventTypes, Sources: r.Sources, Effect: effect})
	}
	return out
}

// impactAcknowledgment identifies one retirement and the exact subscriptions it interrupts.
var acknowledgmentFormat = regexp.MustCompile(`^[0-9a-f]{64}$`)

func impactAcknowledgment(versionID string, rows []db.NotificationSubscription) string {
	if len(rows) == 0 {
		return ""
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	sum := sha256.Sum256([]byte("retire:" + versionID + ":" + strings.Join(ids, ",")))
	return hex.EncodeToString(sum[:])
}

// PolicyTransitionImpact previews what the version's next transition does to notification subscriptions.
// Retiring the active version interrupts every active subscription approved under it. Activating a draft
// restores none: subscriptions approved under an earlier version need explicit reapproval.
func (s Store) PolicyTransitionImpact(ctx context.Context, id string) (PolicyImpact, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return PolicyImpact{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	version, err := q.GetPolicyVersion(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return PolicyImpact{}, ErrNotFound
	}
	if err != nil {
		return PolicyImpact{}, err
	}
	switch version.State {
	case "active":
		rows, err := q.ListSubscriptionsBoundToVersion(ctx, id)
		if err != nil {
			return PolicyImpact{}, err
		}
		return PolicyImpact{PolicyVersionID: id, Transition: "retire", Subscriptions: impacted(rows, "interrupted"),
			Acknowledgment: impactAcknowledgment(id, rows)}, nil
	case "draft":
		rows, err := q.ListSubscriptionsNeedingReapproval(ctx, version.PurposeID)
		if err != nil {
			return PolicyImpact{}, err
		}
		return PolicyImpact{PolicyVersionID: id, Transition: "activate", Subscriptions: impacted(rows, "remains_interrupted")}, nil
	}
	return PolicyImpact{}, problem(409, "No transition", "A retired policy version has no further transition.")
}

// interruptSubscriptions runs inside a retirement, under the purpose lock. Every active subscription
// approved under the version stops authorizing notifications and needs reapproval. When there are any,
// the operator must have acknowledged exactly these; a stale or missing acknowledgment refuses the
// retirement. It returns how many it interrupted.
func interruptSubscriptions(ctx context.Context, q *db.Queries, versionID, acknowledged, actor string) (int, error) {
	if acknowledged != "" && !acknowledgmentFormat.MatchString(acknowledged) {
		return 0, problem(422, "Invalid acknowledgment", "acknowledged_impact must be the acknowledgment the impact returned.")
	}
	rows, err := q.ListSubscriptionsBoundToVersion(ctx, versionID)
	if err != nil {
		return 0, err
	}
	if expected := impactAcknowledgment(versionID, rows); acknowledged != expected {
		if expected == "" {
			return 0, problem(409, "Stale acknowledgment", "The subscriptions this acknowledgment named have changed; read the impact again.")
		}
		return 0, problem(409, "Impact not acknowledged",
			"Retiring this version interrupts notification subscriptions; read its impact and send that acknowledgment as acknowledged_impact.")
	}
	if len(rows) == 0 {
		return 0, nil
	}
	ids := make([]string, len(rows))
	for i, r := range rows {
		ids[i] = r.ID
	}
	if _, err = q.InterruptSubscriptions(ctx, ids); err != nil {
		return 0, err
	}
	for _, id := range ids {
		if err = q.InsertSubscriptionAudit(ctx, db.InsertSubscriptionAuditParams{ID: UUID(), SubscriptionID: id, Action: "interrupted",
			PolicyVersionID: optionalUUID(versionID), Actor: actor}); err != nil {
			return 0, err
		}
	}
	return len(ids), nil
}

// ReapprovalRequest names the new subscription that replaces an interrupted one.
type ReapprovalRequest struct {
	Name string `json:"name"`
	// BasisAssessment is required when the active version's lawful basis is not consent.
	BasisAssessment string `json:"basis_assessment"`
}

// ReapproveSubscription approves, under the purpose's active policy version, a new subscription with the
// interrupted one's scope, and marks the interrupted one superseded. Both are kept. Nothing the
// interrupted subscription missed is sent: that needs a recovery request.
func (s Store) ReapproveSubscription(ctx context.Context, name string, req ReapprovalRequest, actor string) (NotificationSubscription, error) {
	if !profiles.Name.MatchString(req.Name) {
		return NotificationSubscription{}, problem(422, "Invalid reapproval", "name must name the new subscription.")
	}
	old, err := db.New(s.Pool).GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationSubscription{}, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return NotificationSubscription{}, err
	}
	created, err := s.createSubscription(ctx, req.Name, SubscriptionRequest{Destination: old.Destination, PurposeID: old.PurposeID,
		BasisAssessment: req.BasisAssessment, RecipientRole: old.RecipientRole, EventTypes: old.EventTypes, DataCategories: old.DataCategories,
		Sources: old.Sources}, actor, old.ID)
	if err != nil {
		return NotificationSubscription{}, err
	}
	created.Replaces = name
	return created, nil
}

// replace runs inside a reapproval, under the purpose lock, once the new subscription is inserted.
func replace(ctx context.Context, q *db.Queries, replaced string, created db.NotificationSubscription, actor string) error {
	old, err := q.GetNotificationSubscription(ctx, replaced)
	if err != nil {
		return err
	}
	if old.State != "needs_reapproval" && old.State != "suspended" || old.PolicyVersionID == created.PolicyVersionID {
		return problem(409, "Not interrupted", "Only a subscription whose policy version is no longer active can be reapproved.")
	}
	if changed, err := q.SupersedeSubscription(ctx, old.ID); err != nil || changed != 1 {
		if err == nil {
			err = problem(409, "Not interrupted", "The subscription changed concurrently; read it and retry.")
		}
		return err
	}
	for _, a := range []db.InsertSubscriptionAuditParams{
		{ID: UUID(), SubscriptionID: created.ID, Action: "reapproved", PolicyVersionID: optionalUUID(created.PolicyVersionID), RelatedID: optionalUUID(old.ID), Actor: actor},
		{ID: UUID(), SubscriptionID: old.ID, Action: "superseded", PolicyVersionID: optionalUUID(created.PolicyVersionID), RelatedID: optionalUUID(created.ID), Actor: actor},
	} {
		if err = q.InsertSubscriptionAudit(ctx, a); err != nil {
			return err
		}
	}
	return nil
}

// SubscriptionAuditEntry is one change to a subscription's authorization.
type SubscriptionAuditEntry struct {
	Action          string    `json:"action"`
	PolicyVersionID string    `json:"policy_version_id,omitempty"`
	RelatedID       string    `json:"related_id,omitempty"`
	Actor           string    `json:"actor"`
	RecordedAt      time.Time `json:"recorded_at"`
}

func (s Store) SubscriptionAudit(ctx context.Context, name string) ([]SubscriptionAuditEntry, error) {
	q := db.New(s.Pool)
	subscription, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListSubscriptionAudit(ctx, subscription.ID)
	out := make([]SubscriptionAuditEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, SubscriptionAuditEntry{Action: r.Action, PolicyVersionID: uuidString(r.PolicyVersionID), RelatedID: uuidString(r.RelatedID),
			Actor: r.Actor, RecordedAt: r.RecordedAt})
	}
	return out, err
}

// NotificationRecovery is one request to send what a replaced subscription suppressed while it needed
// reapproval. Items counts its events by state: queued and started are sent through the subscription,
// superseded ones are covered by a newer change of the same record, and unrecoverable ones have no
// history left to send. Outcomes counts the recorded results of the started ones.
type NotificationRecovery struct {
	ID           string           `json:"id"`
	Subscription string           `json:"subscription"`
	RequestedBy  string           `json:"requested_by"`
	RequestedAt  time.Time        `json:"requested_at"`
	MorePending  bool             `json:"more_pending"`
	Items        map[string]int64 `json:"items"`
	Outcomes     map[string]int64 `json:"outcomes"`
}

// RequestRecovery queues, for the named subscription, the notifications that the interrupted
// subscription it replaced suppressed because its policy version no longer authorized it. It is
// idempotent per subscription and key, and an event is never queued twice for a subscription, across
// requests. Per record only the newest change is sent: older ones, and ones older than a change the
// subscription already delivered, are superseded. A change whose outbox reference no longer exists, or
// that was suppressed longer than recoveryWindow ago, is unrecoverable. One request takes at most
// recoveryBatch events; more_pending says a further request (with a new key) has more to take.
//
// Queuing sends nothing by itself: the recovery workers send each item through the subscription, and
// each is authorized again from current state immediately before it is sent.
func (s Store) RequestRecovery(ctx context.Context, name, key, actor string) (NotificationRecovery, bool, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NotificationRecovery{}, false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	subscription, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationRecovery{}, false, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return NotificationRecovery{}, false, err
	}
	if err = q.LockPurposePolicies(ctx, subscription.PurposeID); err != nil {
		return NotificationRecovery{}, false, err
	}
	if existing, err := q.GetNotificationRecoveryByKey(ctx, db.GetNotificationRecoveryByKeyParams{SubscriptionID: subscription.ID, IdempotencyKey: key}); err == nil {
		recovery, err := summarize(ctx, q, existing, name)
		return recovery, false, err
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return NotificationRecovery{}, false, err
	}
	if subscription, err = q.GetNotificationSubscription(ctx, subscription.ID); err != nil {
		return NotificationRecovery{}, false, err
	}
	if subscription.State != "active" || !subscription.Replaces.Valid {
		return NotificationRecovery{}, false, problem(409, "Nothing to recover",
			"Only an active subscription that replaced an interrupted one can recover what that one missed.")
	}
	candidates, err := q.ListRecoveryCandidates(ctx, db.ListRecoveryCandidatesParams{Replaced: uuidString(subscription.Replaces), Reasons: interruptionReasons,
		Subscription: subscription.ID, RowLimit: recoveryBatch + 1})
	if err != nil {
		return NotificationRecovery{}, false, err
	}
	more := len(candidates) > recoveryBatch
	candidates = candidates[:min(len(candidates), recoveryBatch)]
	profileIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		profileIDs = append(profileIDs, c.ProfileID)
	}
	notified, err := q.LatestNotifiedVersions(ctx, db.LatestNotifiedVersionsParams{Subscription: subscription.ID, ProfileIds: profileIDs})
	if err != nil {
		return NotificationRecovery{}, false, err
	}
	newest := map[string]int64{}
	for _, n := range notified {
		newest[n.ProfileID] = n.Version
	}
	horizon := time.Now().Add(-recoveryWindow)
	recoverable := func(c db.ListRecoveryCandidatesRow) bool { return c.History && c.RecordedAt.After(horizon) }
	for _, c := range candidates {
		if recoverable(c) && c.ProfileVersion > newest[c.ProfileID] {
			newest[c.ProfileID] = c.ProfileVersion
		}
	}
	recovery, err := q.InsertNotificationRecovery(ctx, db.InsertNotificationRecoveryParams{ID: UUID(), SubscriptionID: subscription.ID,
		IdempotencyKey: key, RequestedBy: actor, MorePending: more})
	if err != nil {
		return NotificationRecovery{}, false, err
	}
	items := db.InsertRecoveryItemsParams{Subscription: subscription.ID, Recovery: recovery.ID}
	for _, c := range candidates {
		state, reason := "queued", ""
		switch {
		case !c.History:
			state, reason = "unrecoverable", "history_unavailable"
		case !c.RecordedAt.After(horizon):
			state, reason = "unrecoverable", "outside_recovery_window"
		case c.ProfileVersion < newest[c.ProfileID]:
			state, reason = "superseded", "newer_change_notified"
		}
		items.EventIds = append(items.EventIds, c.EventID)
		items.ProfileIds = append(items.ProfileIds, c.ProfileID)
		items.Versions = append(items.Versions, c.ProfileVersion)
		items.States = append(items.States, state)
		items.Reasons = append(items.Reasons, reason)
	}
	if _, err = q.InsertRecoveryItems(ctx, items); err != nil {
		return NotificationRecovery{}, false, err
	}
	if err = q.InsertSubscriptionAudit(ctx, db.InsertSubscriptionAuditParams{ID: UUID(), SubscriptionID: subscription.ID, Action: "recovery_requested",
		PolicyVersionID: optionalUUID(subscription.PolicyVersionID), RelatedID: optionalUUID(recovery.ID), Actor: actor}); err != nil {
		return NotificationRecovery{}, false, err
	}
	result, err := summarize(ctx, q, recovery, name)
	if err != nil {
		return NotificationRecovery{}, false, err
	}
	if err = tx.Commit(ctx); err != nil {
		return NotificationRecovery{}, false, err
	}
	slog.InfoContext(ctx, "notification recovery requested", "recovery_id", recovery.ID, "queued", result.Items["queued"])
	return result, true, nil
}

func summarize(ctx context.Context, q *db.Queries, r db.NotificationRecovery, name string) (NotificationRecovery, error) {
	rows, err := q.SummarizeRecovery(ctx, r.ID)
	if err != nil {
		return NotificationRecovery{}, err
	}
	out := NotificationRecovery{ID: r.ID, Subscription: name, RequestedBy: r.RequestedBy, RequestedAt: r.RequestedAt, MorePending: r.MorePending,
		Items: map[string]int64{"queued": 0, "started": 0, "superseded": 0, "unrecoverable": 0}, Outcomes: map[string]int64{}}
	for _, row := range rows {
		out.Items[row.State] += row.Items
		if row.Outcome != "" {
			out.Outcomes[row.Outcome] += row.Items
		}
	}
	return out, nil
}

// Recovery reads one of a subscription's recovery requests, with its current progress.
func (s Store) Recovery(ctx context.Context, name, id string) (NotificationRecovery, error) {
	q := db.New(s.Pool)
	subscription, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationRecovery{}, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return NotificationRecovery{}, err
	}
	recovery, err := q.GetNotificationRecovery(ctx, db.GetNotificationRecoveryParams{ID: id, SubscriptionID: subscription.ID})
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationRecovery{}, problem(404, "Not found", "Unknown recovery.")
	}
	if err != nil {
		return NotificationRecovery{}, err
	}
	return summarize(ctx, q, recovery, name)
}

// RecoveryItem is one event of a recovery: references, state and, once sent, its recorded outcome.
type RecoveryItem struct {
	EventID        string     `json:"event_id"`
	ProfileID      string     `json:"profile_id"`
	ProfileVersion int64      `json:"profile_version"`
	State          string     `json:"state"`
	Reason         string     `json:"reason,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	Outcome        string     `json:"outcome,omitempty"`
	OutcomeReason  string     `json:"outcome_reason,omitempty"`
}

// RecoveryItems pages through a recovery's events in event ID order, after the given event ID.
func (s Store) RecoveryItems(ctx context.Context, name, id, after string, limit int32) ([]RecoveryItem, error) {
	if _, err := s.Recovery(ctx, name, id); err != nil {
		return nil, err
	}
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	}
	rows, err := db.New(s.Pool).ListRecoveryItems(ctx, db.ListRecoveryItemsParams{Recovery: id, After: after, RowLimit: limit})
	out := make([]RecoveryItem, 0, len(rows))
	for _, r := range rows {
		item := RecoveryItem{EventID: r.EventID, ProfileID: r.ProfileID, ProfileVersion: r.ProfileVersion, State: r.State, Reason: r.Reason.String,
			Outcome: r.Outcome.String, OutcomeReason: r.OutcomeReason.String}
		if r.StartedAt.Valid {
			item.StartedAt = &r.StartedAt.Time
		}
		out = append(out, item)
	}
	return out, err
}

// QueuedRecovery is one recovery item ready to be sent through its subscription.
type QueuedRecovery struct {
	Subscription string
	Event        events.Event
}

// QueuedRecoveries returns up to limit queued recovery items with their envelopes rebuilt from the outbox,
// exactly as the relay published them. An item whose outbox reference has gone since it was queued is
// marked unrecoverable instead.
func (s Store) QueuedRecoveries(ctx context.Context, tenant string, limit int32) ([]QueuedRecovery, error) {
	q := db.New(s.Pool)
	rows, err := q.ListQueuedRecoveryItems(ctx, limit)
	if err != nil {
		return nil, err
	}
	out := make([]QueuedRecovery, 0, len(rows))
	for _, r := range rows {
		if !r.Type.Valid {
			if _, err := q.MarkRecoveryItem(ctx, db.MarkRecoveryItemParams{State: "unrecoverable", Reason: pgtype.Text{String: "history_unavailable", Valid: true},
				Subscription: r.SubscriptionID, EventID: r.EventID}); err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, QueuedRecovery{Subscription: r.SubscriptionID, Event: events.Event{Schema: events.Schema, ID: r.EventID, Type: r.Type.String,
			Tenant: tenant, Source: r.Source.String, ProfileID: r.ProfileID, ProfileVersion: r.Version.Int64, OccurredAt: r.OccurredAt.Time.UTC()}})
	}
	return out, nil
}

// MarkRecoveryStarted records that an item's notification workflow exists; it is not started again.
func (s Store) MarkRecoveryStarted(ctx context.Context, subscription, eventID string) error {
	_, err := db.New(s.Pool).MarkRecoveryItem(ctx, db.MarkRecoveryItemParams{State: "started", Subscription: subscription, EventID: eventID})
	return err
}
