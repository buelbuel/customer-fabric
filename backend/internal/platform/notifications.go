package platform

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Notification outcomes, as recorded in notification_deliveries.
const (
	NotificationDelivered  = "delivered"
	NotificationSuppressed = "suppressed"
	NotificationFailed     = "failed"
)

// NotificationResult is what one notification attempt decided. Reason is a fixed code; Attempts counts
// the destination's attempts and is set when the outcome is recorded.
type NotificationResult struct {
	Outcome    string
	Reason     string
	StatusCode int32
	Attempts   int32
}

// ErrNotificationRejected is a destination refusing a notification for good; retrying cannot help.
var ErrNotificationRejected = errors.New("the destination rejected the notification")

// ErrNotificationUnavailable is a failure that a later attempt may not repeat: a transport error, a
// timeout, or a destination answering 408, 429 or 5xx.
var ErrNotificationUnavailable = errors.New("the destination could not take the notification now")

// Notifier tells a subscription's destination that a source record changed, sending the event's opaque
// references only (ADR 0014). It is the final dispatch point: immediately before every attempt it
// decides again, from current committed state, whether the subscription authorizes this notification
// for this record.
type Notifier struct {
	Store  Store
	Egress Egress
	Client *http.Client
}

func NewNotifier(s Store, egress Egress) Notifier {
	return Notifier{Store: s, Egress: egress, Client: egress.Client(10 * time.Second)}
}

// Subscriptions lists the subscriptions that covered an event when it was committed, by ID. Notify
// decides again.
func (n Notifier) Subscriptions(ctx context.Context, e events.Event) ([]string, error) {
	return db.New(n.Store.Pool).ListSubscriptionsForEvent(ctx, db.ListSubscriptionsForEventParams{EventType: e.Type, Source: e.Source, OccurredAt: e.OccurredAt})
}

// Notify sends one event to one subscription's destination, or decides not to. An event already
// recorded for the subscription is never sent again. Otherwise it is suppressed, with a reason code,
// unless all of these hold now:
//   - the subscription exists, is active (not suspended, interrupted by a policy change or superseded),
//     and covers the event's type and source;
//   - it has not delivered a later version of the record;
//   - the record exists, is active and is not restricted;
//   - the subscription's purpose has an active policy version, which records the lawful basis; it is the
//     version the subscription was approved under; and the record's subject permits the purpose under it
//     (see authorizePurpose);
//   - the destination is registered and its URL is on the webhook allowlist.
//
// It returns ErrNotificationRejected or ErrNotificationUnavailable for a failed send.
func (n Notifier) Notify(ctx context.Context, e events.Event, subscriptionID string) (NotificationResult, error) {
	suppressed := func(reason string) (NotificationResult, error) {
		return NotificationResult{Outcome: NotificationSuppressed, Reason: reason}, nil
	}
	q := db.New(n.Store.Pool)
	if recorded, err := q.GetNotificationDelivery(ctx, db.GetNotificationDeliveryParams{EventID: e.ID, SubscriptionID: subscriptionID}); err == nil {
		return NotificationResult{Outcome: recorded.Outcome, Reason: "already_recorded", StatusCode: recorded.StatusCode.Int32}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return NotificationResult{}, err
	}
	// The decision reads one consistent snapshot.
	tx, err := n.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return NotificationResult{}, err
	}
	defer tx.Rollback(ctx)
	q = q.WithTx(tx)
	subscription, err := q.GetNotificationSubscription(ctx, subscriptionID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return suppressed("subscription_removed")
	case err != nil:
		return NotificationResult{}, err
	case subscription.State == "needs_reapproval":
		return suppressed("subscription_needs_reapproval")
	case subscription.State == "superseded":
		return suppressed("subscription_superseded")
	case subscription.State != "active":
		return suppressed("subscription_suspended")
	case !slices.Contains(subscription.EventTypes, e.Type):
		return suppressed("event_type_not_permitted")
	case !slices.Contains(subscription.Sources, e.Source):
		return suppressed("source_not_in_scope")
	}
	// A notification says only that a record changed; once a later version was delivered, an earlier
	// one, arriving late or recovered, would only move the recipient back.
	if newer, err := q.NewerChangeDelivered(ctx, db.NewerChangeDeliveredParams{SubscriptionID: subscription.ID, ProfileID: e.ProfileID,
		ProfileVersion: e.ProfileVersion}); err != nil || newer {
		if err != nil {
			return NotificationResult{}, err
		}
		return suppressed("newer_change_notified")
	}
	state, err := q.GetProfileLifecycleState(ctx, e.ProfileID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return suppressed("record_unavailable")
	case err != nil:
		return NotificationResult{}, err
	case state.LifecycleState != "active":
		return suppressed("record_unavailable")
	case state.Restricted:
		return suppressed("record_restricted")
	}
	if reason, err := authorizePurpose(ctx, q, e.ProfileID, subscription.PurposeID, subscription.PolicyVersionID); err != nil || reason != "" {
		if err != nil {
			return NotificationResult{}, err
		}
		return suppressed(reason)
	}
	destination, err := q.GetDestination(ctx, subscription.Destination)
	if errors.Is(err, pgx.ErrNoRows) {
		return suppressed("destination_not_registered")
	}
	if err != nil {
		return NotificationResult{}, err
	}
	if !n.Egress.Allows(destination.Url) {
		return suppressed("destination_not_allowed")
	}
	// A secret the configured keys cannot open is our own failure, retried by the caller; it is never
	// sent unsigned.
	secrets, err := n.Store.signingSecrets(ctx, q, destination)
	if err != nil {
		return NotificationResult{}, err
	}
	if err = tx.Rollback(ctx); err != nil {
		return NotificationResult{}, err
	}
	return n.send(ctx, destination, secrets, e)
}

// authorizePurpose decides whether a source record's subject permits a purpose now. It returns an empty
// reason when it does. The purpose needs an active policy version, the one approvedVersion names, whose
// lawful basis governs. A record can have several record lifetimes (one per identifier), and every one of
// them must permit the purpose:
//   - a lifetime verifiably associated with a customer now follows that customer's effective permission,
//     which already includes protective decisions made on the record; the customer must also be active
//     and not restricted;
//   - any other lifetime, including one whose association was revoked, follows the evidence on its own
//     record subject alone, judged by the same engine. Under consent it needs a verified source grant (ADR
//     0015); operator grants and unverified source grants never count. A non-consent basis is permitted
//     unless objected to.
//
// Missing or ambiguous authorization fails closed.
func authorizePurpose(ctx context.Context, q *db.Queries, recordID, purposeID, approvedVersion string) (string, error) {
	active, err := q.GetActivePolicyForPurpose(ctx, purposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return "no_lawful_basis", nil
	}
	if err != nil {
		return "", err
	}
	if active.ID != approvedVersion {
		return "basis_not_approved", nil
	}
	scope, err := q.ListRecordConsentScope(ctx, recordID)
	if err != nil {
		return "", err
	}
	// A record created without any identifier has no lifetime, and so no subject, to authorize.
	if len(scope) == 0 {
		return "record_without_identifier", nil
	}
	var customers []customerAnchor
	// Each unlinked lifetime is decided on its own evidence; "" marks a lifetime without a subject.
	var unlinked []string
	for _, s := range scope {
		if s.SubjectID.Valid && s.SubjectState.String != "active" {
			return "record_subject_unavailable", nil
		}
		if s.CustomerSubject.Valid {
			if !s.AnchorProfileID.Valid {
				return "customer_unavailable", nil
			}
			customers = append(customers, customerAnchor{ProfileID: uuidString(s.AnchorProfileID), Kratos: uuidString(s.CustomerSubject)})
			continue
		}
		unlinked = append(unlinked, "")
		if s.SubjectID.Valid {
			unlinked[len(unlinked)-1] = uuidString(s.SubjectID)
		}
	}
	if len(customers) > 0 {
		for _, c := range customers {
			anchor, err := q.GetProfileLifecycleState(ctx, c.ProfileID)
			if errors.Is(err, pgx.ErrNoRows) || err == nil && (anchor.LifecycleState != "active" || anchor.Restricted) {
				return "customer_unavailable", nil
			}
			if err != nil {
				return "", err
			}
		}
		states, err := purposeStatesFor(ctx, q, customers, purposeID)
		if err != nil {
			return "", err
		}
		for _, s := range states {
			if s.Evaluation.Outcome != consent.Permitted {
				return "purpose_not_permitted", nil
			}
		}
	}
	if len(unlinked) == 0 {
		return "", nil
	}
	rows, err := q.ListPolicyVersionsForPurpose(ctx, purposeID)
	if err != nil {
		return "", err
	}
	versions := make([]consent.PolicyVersion, 0, len(rows))
	for _, r := range rows {
		versions = append(versions, consent.PolicyVersion{Version: r.Version, LawfulBasis: consent.LawfulBasis(r.LawfulBasis), CarryForward: r.CarryForward})
	}
	evidence := map[string][]consent.Evidence{}
	if subjects := slices.DeleteFunc(slices.Clone(unlinked), func(id string) bool { return id == "" }); len(subjects) > 0 {
		recorded, err := q.ListRecordSubjectEvidence(ctx, db.ListRecordSubjectEvidenceParams{SubjectIds: subjects, PurposeID: purposeID, RowLimit: evidenceLimit + 1})
		if err != nil {
			return "", err
		}
		if len(recorded) > evidenceLimit {
			return "purpose_not_permitted", nil
		}
		for _, r := range recorded {
			evidence[r.SubjectID] = append(evidence[r.SubjectID], consent.Evidence{Decision: consent.Decision(r.Decision), EffectiveAt: r.ReceivedAt,
				ActorKind: r.ActorKind, Quarantined: r.Quarantined, Version: r.Version, RecordedBasis: consent.LawfulBasis(r.LawfulBasis),
				VerifiedSource: r.VerifiedSource})
		}
	}
	// One lifetime's grant never speaks for another: every lifetime must permit on its own evidence, and
	// one without a subject has none.
	policy := consent.PolicyVersion{Version: active.Version, LawfulBasis: consent.LawfulBasis(active.LawfulBasis), CarryForward: active.CarryForward}
	for _, subject := range unlinked {
		if consent.Evaluate(policy, versions, evidence[subject]).Outcome != consent.Permitted {
			return "purpose_not_permitted", nil
		}
	}
	return "", nil
}

func uuidString(u pgtype.UUID) string {
	v, _ := u.Value()
	s, _ := v.(string)
	return s
}

// send posts the event to the destination, signed with its live secrets and keyed by the event ID.
func (n Notifier) send(ctx context.Context, destination db.Destination, secrets [][]byte, e events.Event) (NotificationResult, error) {
	body, err := json.Marshal(e)
	if err != nil {
		return NotificationResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.Url, bytes.NewReader(body))
	if err != nil {
		return NotificationResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", e.ID)
	req.Header.Set("CF-Signature", signature(secrets, time.Now(), body))
	res, err := n.Client.Do(req)
	if errors.Is(err, errEgressRefused) {
		// The destination's name resolved to an address the egress policy refuses: nothing was sent.
		return NotificationResult{Outcome: NotificationFailed, Reason: "destination_address_refused"}, nil
	}
	if err != nil {
		// A send cut short by our own shutdown says nothing about the destination.
		if ctx.Err() != nil {
			return NotificationResult{}, ctx.Err()
		}
		return NotificationResult{}, ErrNotificationUnavailable
	}
	res.Body.Close()
	code := int32(res.StatusCode)
	switch {
	case code >= 200 && code < 300:
		return NotificationResult{Outcome: NotificationDelivered, Reason: "accepted", StatusCode: code}, nil
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return NotificationResult{StatusCode: code}, ErrNotificationUnavailable
	}
	return NotificationResult{StatusCode: code}, ErrNotificationRejected
}

// Record stores an event's outcome for a subscription once; recording the same pair again changes nothing.
func (n Notifier) Record(ctx context.Context, e events.Event, subscriptionID string, r NotificationResult) error {
	code := pgtype.Int4{Int32: r.StatusCode, Valid: r.StatusCode != 0}
	_, err := db.New(n.Store.Pool).RecordNotificationDelivery(ctx, db.RecordNotificationDeliveryParams{EventID: e.ID, SubscriptionID: subscriptionID,
		ProfileID: e.ProfileID, ProfileVersion: e.ProfileVersion, Outcome: r.Outcome, Reason: r.Reason, Attempts: r.Attempts, StatusCode: code})
	return err
}
