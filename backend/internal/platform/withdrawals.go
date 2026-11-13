package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Withdrawal notices (ADR 0014). A notification is decided once, just before it is sent, so authorization
// that ends later, or while a send is in flight, never reaches the recipient that way. A subscription
// that delivered a change of a record holds it; when the record is no longer authorized for the
// subscription's purpose, its destination is sent a signed notice to stop using it. The decision is the
// notifier's own, re-taken on current state.

// WithdrawalSchema names the notice format; a recipient refuses one it does not know.
const WithdrawalSchema = "customer-fabric.withdrawal.v1"

// withdrawalReason is why a record is no longer authorized for a subscription's purpose, or empty while
// it is. It concerns the record: suspending or interrupting a subscription stops changes, it does not
// withdraw what was sent. The purpose's active policy version decides, whichever one the subscription
// was approved under.
func withdrawalReason(ctx context.Context, q *db.Queries, profileID, purposeID string) (string, error) {
	state, err := q.GetProfileLifecycleState(ctx, profileID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return "record_unavailable", nil
	case err != nil:
		return "", err
	case state.LifecycleState != "active":
		return "record_unavailable", nil
	case state.Restricted:
		return "record_restricted", nil
	}
	active, err := q.GetActivePolicyForPurpose(ctx, purposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "no_lawful_basis", nil
	}
	if err != nil {
		return "", err
	}
	return authorizePurpose(ctx, q, profileID, purposeID, active.ID)
}

// reviewHolds records a notice for every subscription holding the record that may no longer use it.
func reviewHolds(ctx context.Context, q *db.Queries, profileID string) error {
	held, err := q.HeldRecords(ctx, profileID)
	if err != nil {
		return err
	}
	for _, h := range held {
		subscription, err := q.GetNotificationSubscription(ctx, h.SubscriptionID)
		if err != nil {
			return err
		}
		reason, err := withdrawalReason(ctx, q, profileID, subscription.PurposeID)
		if err != nil || reason == "" {
			return err
		}
		if err = q.InsertWithdrawal(ctx, db.InsertWithdrawalParams{ID: UUID(), SubscriptionID: h.SubscriptionID, ProfileID: profileID,
			HeldVersion: h.Version, Reason: reason}); err != nil {
			return err
		}
	}
	return nil
}

// Withdrawals reviews held records and sends due notices. Like activation deliveries, each notice is
// decided again immediately before every attempt and retried with backoff up to a bound.
type Withdrawals struct {
	Store       Store
	Client      *http.Client
	Egress      Egress
	Tenant      string
	MaxAttempts int32
	Lease       time.Duration
	Backoff     func(attempt int32) time.Duration
	// SweepBatch is how many held records one pass re-reviews, continuing where the last pass ended.
	SweepBatch int32
	cursor     *string
}

// NewWithdrawals returns a worker that retries a notice for about a day before failing it.
func NewWithdrawals(s Store, egress Egress, tenant string) Withdrawals {
	cursor := ""
	return Withdrawals{
		Store: s, Egress: egress, Tenant: tenant, MaxAttempts: 12, Lease: time.Minute, SweepBatch: 200, cursor: &cursor,
		Client: egress.Client(10 * time.Second),
		Backoff: func(attempt int32) time.Duration {
			return min(time.Duration(1<<min(attempt, 12))*time.Second, time.Hour)
		},
	}
}

// Run reviews and sends every interval until ctx ends. A failed pass is logged and repeated; queued
// reviews and pending notices stay where they are.
func (w Withdrawals) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if err := w.Pass(ctx); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "withdrawal pass failed; retrying next interval", "error_type", fmt.Sprintf("%T", err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// Pass reviews the queued records, sweeps the next batch of held records, then sends due notices.
func (w Withdrawals) Pass(ctx context.Context) error {
	if _, err := w.ReviewQueued(ctx); err != nil {
		return err
	}
	if err := w.Sweep(ctx); err != nil {
		return err
	}
	_, err := w.SendDue(ctx, 50)
	return err
}

// ReviewQueued reviews queued records, one per transaction, until the queue is empty.
func (w Withdrawals) ReviewQueued(ctx context.Context) (int, error) {
	n := 0
	for ctx.Err() == nil {
		done, err := w.reviewNext(ctx)
		if err != nil || !done {
			return n, err
		}
		n++
	}
	return n, ctx.Err()
}

func (w Withdrawals) reviewNext(ctx context.Context) (bool, error) {
	// One snapshot decides the record; a conflicting concurrent change leaves it queued for the next pass.
	tx, err := w.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx)
	q := db.New(w.Store.Pool).WithTx(tx)
	profileID, err := q.ClaimReview(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = reviewHolds(ctx, q, profileID); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}

// Sweep re-reviews the next batch of records some subscription delivered. It covers what no write
// announces, such as a grant whose verification became overdue, and wraps around at the end.
func (w Withdrawals) Sweep(ctx context.Context) error {
	if w.cursor == nil {
		w.cursor = new(string)
	}
	after := *w.cursor
	if after == "" {
		after = "00000000-0000-0000-0000-000000000000"
	}
	ids, err := db.New(w.Store.Pool).NextHeldProfiles(ctx, db.NextHeldProfilesParams{After: after, RowLimit: w.SweepBatch})
	if err != nil {
		return err
	}
	for _, id := range ids {
		tx, err := w.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
		if err != nil {
			return err
		}
		if err = reviewHolds(ctx, db.New(w.Store.Pool).WithTx(tx), id); err == nil {
			err = tx.Commit(ctx)
		}
		_ = tx.Rollback(ctx)
		if err != nil {
			return err
		}
		*w.cursor = id
	}
	if int32(len(ids)) < w.SweepBatch {
		*w.cursor = ""
	}
	return nil
}

// SendDue sends up to limit due notices, claiming each just before deciding and sending it.
func (w Withdrawals) SendDue(ctx context.Context, limit int) (int, error) {
	claimed := 0
	for claimed < limit && ctx.Err() == nil {
		notice, err := db.New(w.Store.Pool).ClaimDueWithdrawal(ctx, int32(w.Lease/time.Second))
		if errors.Is(err, pgx.ErrNoRows) {
			break
		}
		if err != nil {
			return claimed, err
		}
		claimed++
		if err = w.send(ctx, notice); err != nil {
			slog.ErrorContext(ctx, "withdrawal notice could not be sent", "withdrawal_id", notice.ID, "attempt", notice.Attempts)
		}
	}
	return claimed, nil
}

// withdrawalNotice is what a destination receives: references and a reason code, never values.
type withdrawalNotice struct {
	Schema             string    `json:"schema"`
	ID                 string    `json:"id"`
	Type               string    `json:"type"`
	Tenant             string    `json:"tenant"`
	Subscription       string    `json:"subscription"`
	ProfileID          string    `json:"profile_id"`
	LastProfileVersion int64     `json:"last_profile_version"`
	Reason             string    `json:"reason"`
	OccurredAt         time.Time `json:"occurred_at"`
}

// decide re-takes a notice's decision in one snapshot. outcome is empty when it is to be sent, and the
// body, target and secrets are then set.
func (w Withdrawals) decide(ctx context.Context, n db.NotificationWithdrawal) (outcome, reason string, body []byte, target string, secrets [][]byte, err error) {
	tx, err := w.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", "", nil, "", nil, err
	}
	defer tx.Rollback(ctx)
	q := db.New(w.Store.Pool).WithTx(tx)
	subscription, err := q.GetNotificationSubscription(ctx, n.SubscriptionID)
	if err != nil {
		return "", "", nil, "", nil, err
	}
	// Authorized again before the notice went out: the recipient may keep using the record.
	if current, err := withdrawalReason(ctx, q, n.ProfileID, subscription.PurposeID); err != nil || current == "" {
		return "cancelled", "authorized_again", nil, "", nil, err
	}
	destination, err := q.GetDestination(ctx, subscription.Destination)
	if errors.Is(err, pgx.ErrNoRows) {
		return "failed", "destination_not_registered", nil, "", nil, nil
	}
	if err != nil {
		return "", "", nil, "", nil, err
	}
	if !w.Egress.Allows(destination.Url) {
		return "failed", "destination_not_allowed", nil, "", nil, nil
	}
	if secrets, err = w.Store.signingSecrets(ctx, q, destination.Name); err != nil {
		return "", "", nil, "", nil, err
	}
	body, err = json.Marshal(withdrawalNotice{Schema: WithdrawalSchema, ID: n.ID, Type: "notification.withdrawn.v1", Tenant: w.Tenant,
		Subscription: subscription.Name, ProfileID: n.ProfileID, LastProfileVersion: n.HeldVersion, Reason: n.Reason, OccurredAt: n.CreatedAt.UTC()})
	return "", "", body, destination.Url, secrets, err
}

func (w Withdrawals) send(ctx context.Context, n db.NotificationWithdrawal) error {
	limit := n.AttemptBase + w.MaxAttempts
	if n.Attempts > limit {
		return w.settle(ctx, n, "failed", "failed", "retries_exhausted", 0, time.Now())
	}
	retry := func(reason string, code int32) error {
		if n.Attempts >= limit {
			return w.settle(ctx, n, "failed", "failed", reason, code, time.Now())
		}
		return w.settle(ctx, n, "pending", "retry", reason, code, time.Now().Add(w.Backoff(n.Attempts)))
	}
	outcome, reason, body, target, secrets, err := w.decide(ctx, n)
	if errors.Is(err, ErrSigningKeyUnavailable) {
		// Never sent unsigned: the notice waits for the key within its attempts and can be redriven.
		return retry("signing_key_unavailable", 0)
	}
	if err != nil {
		if n.Attempts >= limit {
			return errors.Join(err, w.settle(ctx, n, "failed", "failed", "dispatch_error", 0, time.Now()))
		}
		return err
	}
	if outcome != "" {
		return w.settle(ctx, n, outcome, outcome, reason, 0, time.Now())
	}
	// The notice ID is the idempotency key, so a resend after a lost answer is recognisable.
	sent, code, err := postWebhook(ctx, w.Client, target, n.ID, http.Header{"Cf-Delivery-Attempt": {strconv.Itoa(int(n.Attempts))}}, secrets, body)
	if err != nil {
		return err
	}
	switch sent {
	case webhookRefused:
		return w.settle(ctx, n, "failed", "failed", "destination_address_refused", 0, time.Now())
	case webhookAccepted:
		return w.settle(ctx, n, "delivered", "delivered", "accepted", code, time.Now())
	case webhookRejected:
		return w.settle(ctx, n, "failed", "failed", "rejected", code, time.Now())
	}
	if code == 0 {
		return retry("transport_error", 0)
	}
	return retry("transient_status", code)
}

// settle records an attempt only while this worker still holds the claim.
func (w Withdrawals) settle(ctx context.Context, n db.NotificationWithdrawal, state, outcome, reason string, code int32, next time.Time) error {
	ctx = context.WithoutCancel(ctx)
	tx, err := w.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(w.Store.Pool).WithTx(tx)
	settled, err := q.SettleWithdrawal(ctx, db.SettleWithdrawalParams{State: state, NextAttemptAt: next, ID: n.ID, Attempt: n.Attempts})
	if err != nil {
		return err
	}
	if settled == 0 {
		slog.WarnContext(ctx, "withdrawal lease lost before settling", "withdrawal_id", n.ID)
		return nil
	}
	if err = q.InsertWithdrawalAttempt(ctx, db.InsertWithdrawalAttemptParams{WithdrawalID: n.ID, Attempt: n.Attempts, Outcome: outcome,
		Reason: reason, StatusCode: pgtype.Int4{Int32: code, Valid: code != 0}}); err != nil {
		return err
	}
	slog.InfoContext(ctx, "withdrawal notice settled", "withdrawal_id", n.ID, "attempt", n.Attempts, "outcome", outcome, "reason", reason)
	return tx.Commit(ctx)
}

// WithdrawalAttempt is one recorded attempt or decision on a notice.
type WithdrawalAttempt struct {
	Attempt    int32     `json:"attempt"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason"`
	StatusCode *int32    `json:"status_code,omitempty"`
	RecordedAt time.Time `json:"recorded_at"`
}

// Withdrawal is a notice and its attempts.
type Withdrawal struct {
	ID                 string              `json:"id"`
	ProfileID          string              `json:"profile_id"`
	LastProfileVersion int64               `json:"last_profile_version"`
	Reason             string              `json:"reason"`
	State              string              `json:"state"`
	CreatedAt          time.Time           `json:"created_at"`
	SettledAt          *time.Time          `json:"settled_at,omitempty"`
	Attempts           []WithdrawalAttempt `json:"attempts"`
}

// SubscriptionWithdrawals lists a subscription's 100 most recent notices with their attempts.
func (s Store) SubscriptionWithdrawals(ctx context.Context, name string) ([]Withdrawal, error) {
	q := db.New(s.Pool)
	subscription, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListWithdrawals(ctx, subscription.ID)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	attempts, err := q.ListWithdrawalAttempts(ctx, ids)
	if err != nil {
		return nil, err
	}
	byNotice := map[string][]WithdrawalAttempt{}
	for _, a := range attempts {
		attempt := WithdrawalAttempt{Attempt: a.Attempt, Outcome: a.Outcome, Reason: a.Reason, RecordedAt: a.RecordedAt}
		if a.StatusCode.Valid {
			attempt.StatusCode = &a.StatusCode.Int32
		}
		byNotice[a.WithdrawalID] = append(byNotice[a.WithdrawalID], attempt)
	}
	out := make([]Withdrawal, 0, len(rows))
	for _, r := range rows {
		item := Withdrawal{ID: r.ID, ProfileID: r.ProfileID, LastProfileVersion: r.HeldVersion, Reason: r.Reason, State: r.State,
			CreatedAt: r.CreatedAt, Attempts: byNotice[r.ID]}
		if item.Attempts == nil {
			item.Attempts = []WithdrawalAttempt{}
		}
		if r.SettledAt.Valid {
			item.SettledAt = &r.SettledAt.Time
		}
		out = append(out, item)
	}
	return out, nil
}

// RedriveWithdrawals gives a subscription's failed notices a fresh attempt budget and returns how many.
func (s Store) RedriveWithdrawals(ctx context.Context, name string) (int64, error) {
	q := db.New(s.Pool)
	subscription, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return 0, err
	}
	return q.RedriveFailedWithdrawals(ctx, subscription.ID)
}
