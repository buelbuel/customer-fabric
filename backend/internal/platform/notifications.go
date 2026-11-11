package platform

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/events"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Notification outcomes, as recorded in profile_change_notifications.
const (
	NotificationDelivered  = "delivered"
	NotificationSuppressed = "suppressed"
	NotificationFailed     = "failed"
)

// NotificationResult is what one notification attempt decided. Reason is a fixed code.
type NotificationResult struct {
	Outcome    string
	Reason     string
	StatusCode int32
}

// ErrNotificationRejected is a destination refusing a notification for good; retrying cannot help.
var ErrNotificationRejected = errors.New("the destination rejected the notification")

// ErrNotificationUnavailable is a failure that a later attempt may not repeat: a transport error, a
// timeout, or a destination answering 408, 429 or 5xx.
var ErrNotificationUnavailable = errors.New("the destination could not take the notification now")

// Notifier tells one registered destination that a source record changed, sending the event's opaque
// references only (ADR 0014). It is the final dispatch point, so it decides again from current
// committed state immediately before every attempt.
type Notifier struct {
	Store       Store
	Hosts       []string
	Destination string
	Client      *http.Client
}

func NewNotifier(s Store, hosts []string, destination string) Notifier {
	return Notifier{Store: s, Hosts: hosts, Destination: destination,
		// No proxy and no redirects, as for activation deliveries.
		Client: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

// Notify sends one event, or decides not to. An event already recorded is never sent again; a record
// that is no longer active, is restricted, or a destination that is not registered or not allowed,
// suppresses it. It returns ErrNotificationRejected or ErrNotificationUnavailable for a failed send.
func (n Notifier) Notify(ctx context.Context, e events.Event) (NotificationResult, error) {
	q := db.New(n.Store.Pool)
	if recorded, err := q.GetProfileChangeNotification(ctx, e.ID); err == nil {
		return NotificationResult{Outcome: recorded.Outcome, Reason: "already_recorded", StatusCode: recorded.StatusCode.Int32}, nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return NotificationResult{}, err
	}
	state, err := q.GetProfileLifecycleState(ctx, e.ProfileID)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return NotificationResult{Outcome: NotificationSuppressed, Reason: "record_unavailable"}, nil
	case err != nil:
		return NotificationResult{}, err
	case state.LifecycleState != "active":
		return NotificationResult{Outcome: NotificationSuppressed, Reason: "record_unavailable"}, nil
	case state.Restricted:
		return NotificationResult{Outcome: NotificationSuppressed, Reason: "record_restricted"}, nil
	}
	destination, err := q.GetDestination(ctx, n.Destination)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationResult{Outcome: NotificationSuppressed, Reason: "destination_not_registered"}, nil
	}
	if err != nil {
		return NotificationResult{}, err
	}
	if !allowedWebhook(destination.Url, n.Hosts) {
		return NotificationResult{Outcome: NotificationSuppressed, Reason: "destination_not_allowed"}, nil
	}
	body, err := json.Marshal(e)
	if err != nil {
		return NotificationResult{}, err
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	mac := hmac.New(sha256.New, destination.SigningSecret)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, destination.Url, bytes.NewReader(body))
	if err != nil {
		return NotificationResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", e.ID)
	req.Header.Set("CF-Signature", "t="+timestamp+",v1="+hex.EncodeToString(mac.Sum(nil)))
	res, err := n.Client.Do(req)
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

// Record stores an event's outcome once; recording the same event again changes nothing.
func (n Notifier) Record(ctx context.Context, e events.Event, r NotificationResult) error {
	code := pgtype.Int4{Int32: r.StatusCode, Valid: r.StatusCode != 0}
	_, err := db.New(n.Store.Pool).RecordProfileChangeNotification(ctx, db.RecordProfileChangeNotificationParams{EventID: e.ID,
		ProfileID: e.ProfileID, ProfileVersion: e.ProfileVersion, Destination: n.Destination, Outcome: r.Outcome, Reason: r.Reason, StatusCode: code})
	return err
}
