package platform

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// Destination is a test webhook. SigningSecret is set only in the response that created it.
type Destination struct {
	Name          string    `json:"name"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"created_at"`
	SigningSecret string    `json:"signing_secret,omitempty"`
}

// CreateDestination registers a webhook and returns its first signing secret once.
func (s Store) CreateDestination(ctx context.Context, name, rawURL string, egress Egress) (Destination, error) {
	d := Destination{Name: name, URL: rawURL}
	if !egress.Allows(rawURL) {
		return d, problem(422, "Invalid destination", "The URL must be https (or http where configured) with an explicit port on a configured webhook host, without credentials, query or fragment.")
	}
	if !s.Keys.Configured() {
		return d, errKeyProblem
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		created, err := q.InsertDestination(ctx, db.InsertDestinationParams{Name: name, Url: rawURL})
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(409, "Destination exists", "Destinations are immutable; register a new name.")
		}
		if err != nil {
			return err
		}
		secret, err := s.Keys.issueSecret(ctx, q, name, 1)
		d.CreatedAt, d.SigningSecret = created, hex.EncodeToString(secret)
		return err
	})
	if errors.Is(err, ErrSigningKeyUnavailable) {
		return Destination{Name: name, URL: rawURL}, errKeyProblem
	}
	if err != nil {
		return Destination{Name: name, URL: rawURL}, err
	}
	return d, nil
}

func (s Store) Destinations(ctx context.Context) ([]Destination, error) {
	rows, err := db.New(s.Pool).ListDestinations(ctx)
	out := make([]Destination, 0, len(rows))
	for _, r := range rows {
		out = append(out, Destination{Name: r.Name, URL: r.Url, CreatedAt: r.CreatedAt})
	}
	return out, err
}

// ActivationSummary is what an activation queued. Members are queued; nothing is sent yet.
type ActivationSummary struct {
	ID          string          `json:"id"`
	Segment     string          `json:"segment"`
	Revision    int32           `json:"revision"`
	Destination string          `json:"destination"`
	PurposeID   string          `json:"purpose_id"`
	CreatedAt   time.Time       `json:"created_at"`
	Population  int             `json:"population"`
	Queued      int             `json:"queued"`
	Excluded    SegmentExcluded `json:"excluded"`
}

// Activate evaluates a segment's current revision and queues one delivery per member in the same
// transaction. Queuing is not permission to send: the dispatcher decides each delivery again.
func (s Store) Activate(ctx context.Context, segment, destination string) (ActivationSummary, error) {
	return bounded(ctx, s, func(ctx context.Context) (ActivationSummary, error) { return s.activate(ctx, segment, destination) })
}

func (s Store) activate(ctx context.Context, segment, destination string) (ActivationSummary, error) {
	var a ActivationSummary
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return a, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := q.GetCurrentSegment(ctx, segment)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, problem(404, "Not found", "The segment is not defined.")
	}
	if err != nil {
		return a, err
	}
	if _, err = q.GetDestination(ctx, destination); errors.Is(err, pgx.ErrNoRows) {
		return a, problem(422, "Invalid activation", "destination must be a registered destination.")
	} else if err != nil {
		return a, err
	}
	rule, err := governedRule(ctx, q, row)
	if err != nil {
		return a, err
	}
	members, err := evaluatePopulation(ctx, q, rule, time.Now().UTC(), walk{})
	if err != nil {
		return a, err
	}
	if a, err = storeActivation(ctx, q, row, rule.PurposeID, destination, members); err != nil {
		return a, err
	}
	// The commit is outside the deadline: a 503 must mean nothing was written, never "maybe committed".
	return a, tx.Commit(context.WithoutCancel(ctx))
}

// storeActivation queues one delivery per member of an evaluated population for a destination, through
// q. Queuing is not permission to send: the dispatcher decides each delivery again before every attempt.
func storeActivation(ctx context.Context, q *db.Queries, row db.SegmentDefinition, purposeID, destination string, members population) (ActivationSummary, error) {
	a := ActivationSummary{ID: UUID(), Segment: row.Name, Revision: row.Revision, Destination: destination, PurposeID: purposeID,
		Population: members.Size, Queued: len(members.Members), Excluded: members.Excluded}
	var err error
	if a.CreatedAt, err = q.InsertActivation(ctx, db.InsertActivationParams{ID: a.ID, SegmentName: row.Name, SegmentRevision: row.Revision,
		Destination: destination, PurposeID: purposeID}); err != nil {
		return a, err
	}
	for chunk := range slices.Chunk(members.Members, insertChunk) {
		ids := make([]string, len(chunk))
		for i := range ids {
			ids[i] = UUID()
		}
		if err = q.InsertDeliveries(ctx, db.InsertDeliveriesParams{ActivationID: a.ID, Ids: ids, AnchorIds: chunk}); err != nil {
			return a, err
		}
	}
	return a, nil
}

// ActivationStatus lists each delivery's state and receipts. Receipts hold decisions and status codes,
// never payloads.
type ActivationStatus struct {
	ID          string    `json:"id"`
	Segment     string    `json:"segment"`
	Revision    int32     `json:"revision"`
	Destination string    `json:"destination"`
	PurposeID   string    `json:"purpose_id"`
	CreatedAt   time.Time `json:"created_at"`
	// Counts holds deliveries per state; failed deliveries can be redriven.
	Counts     map[string]int   `json:"counts"`
	Deliveries []DeliveryStatus `json:"deliveries"`
	// NextCursor pages Deliveries in anchor order while more remain.
	NextCursor string `json:"next_cursor,omitempty"`
}

type DeliveryStatus struct {
	ID              string    `json:"id"`
	AnchorProfileID string    `json:"anchor_profile_id"`
	State           string    `json:"state"`
	Attempts        int32     `json:"attempts"`
	Redrives        int32     `json:"redrives"`
	Reason          string    `json:"reason,omitempty"`
	UpdatedAt       time.Time `json:"updated_at"`
	Receipts        []Receipt `json:"receipts"`
}

type Receipt struct {
	Attempt    int32     `json:"attempt"`
	Outcome    string    `json:"outcome"`
	Reason     string    `json:"reason"`
	StatusCode *int32    `json:"status_code,omitempty"`
	At         time.Time `json:"at"`
}

// Activation returns an activation's counts and one page of its deliveries after the anchor cursor.
func (s Store) Activation(ctx context.Context, id, cursor string, limit int) (ActivationStatus, error) {
	var status ActivationStatus
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return status, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	a, err := q.GetActivation(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return status, problem(404, "Not found", "Unknown activation.")
	}
	if err != nil {
		return status, err
	}
	if cursor == "" {
		cursor = "00000000-0000-0000-0000-000000000000"
	}
	deliveries, err := q.ListActivationDeliveries(ctx, db.ListActivationDeliveriesParams{ActivationID: id, After: cursor, RowLimit: int32(limit) + 1})
	if err != nil {
		return status, err
	}
	next := ""
	if len(deliveries) > limit {
		deliveries = deliveries[:limit]
		next = deliveries[limit-1].AnchorProfileID
	}
	ids := make([]string, len(deliveries))
	for i, d := range deliveries {
		ids[i] = d.ID
	}
	receipts, err := q.ListActivationReceipts(ctx, ids)
	if err != nil {
		return status, err
	}
	byDelivery := map[string][]Receipt{}
	for _, r := range receipts {
		receipt := Receipt{Attempt: r.Attempt, Outcome: r.Outcome, Reason: r.Reason, At: r.At}
		if r.StatusCode.Valid {
			receipt.StatusCode = &r.StatusCode.Int32
		}
		byDelivery[r.DeliveryID] = append(byDelivery[r.DeliveryID], receipt)
	}
	counts, err := q.CountActivationStates(ctx, id)
	if err != nil {
		return status, err
	}
	status = ActivationStatus{ID: a.ID, Segment: a.SegmentName, Revision: a.SegmentRevision, Destination: a.Destination, PurposeID: a.PurposeID,
		CreatedAt: a.CreatedAt, Counts: map[string]int{"pending": 0, "delivered": 0, "suppressed": 0, "failed": 0},
		Deliveries: make([]DeliveryStatus, 0, len(deliveries)), NextCursor: next}
	for _, c := range counts {
		status.Counts[c.State] = int(c.Deliveries)
	}
	for _, d := range deliveries {
		receipts := byDelivery[d.ID]
		if receipts == nil {
			receipts = []Receipt{}
		}
		status.Deliveries = append(status.Deliveries, DeliveryStatus{ID: d.ID, AnchorProfileID: d.AnchorProfileID, State: d.State, Attempts: d.Attempts,
			Redrives: d.Redrives, Reason: d.Reason.String, UpdatedAt: d.UpdatedAt, Receipts: receipts})
	}
	return status, nil
}

// ActivationSummaryItem is one recent activation with its deliveries per state.
type ActivationSummaryItem struct {
	ID          string         `json:"id"`
	Segment     string         `json:"segment"`
	Revision    int32          `json:"revision"`
	Destination string         `json:"destination"`
	CreatedAt   time.Time      `json:"created_at"`
	Counts      map[string]int `json:"counts"`
}

// RecentActivations lists the 50 most recent activations, newest first.
func (s Store) RecentActivations(ctx context.Context) ([]ActivationSummaryItem, error) {
	rows, err := db.New(s.Pool).ListRecentActivations(ctx)
	out := make([]ActivationSummaryItem, 0, len(rows))
	for _, r := range rows {
		out = append(out, ActivationSummaryItem{ID: r.ID, Segment: r.SegmentName, Revision: r.SegmentRevision, Destination: r.Destination, CreatedAt: r.CreatedAt,
			Counts: map[string]int{"pending": int(r.Pending), "delivered": int(r.Delivered), "suppressed": int(r.Suppressed), "failed": int(r.Failed)}})
	}
	return out, err
}

// RedriveFailed requeues an activation's failed deliveries with a fresh attempt budget and returns how
// many it requeued. They are dispatched through every gate again, so a customer who withdrew meanwhile is
// suppressed, and suppressed deliveries are never requeued.
func (s Store) RedriveFailed(ctx context.Context, id string) (int64, error) {
	q := db.New(s.Pool)
	if _, err := q.GetActivation(ctx, id); errors.Is(err, pgx.ErrNoRows) {
		return 0, problem(404, "Not found", "Unknown activation.")
	} else if err != nil {
		return 0, err
	}
	return q.RedriveFailedDeliveries(ctx, id)
}

// Dispatcher sends due deliveries. Immediately before each send it re-decides the delivery from current
// data: destination allowlist, segment governance, the customer's lifecycle and view, effective
// permission for the purpose and segment criteria. Any failed gate suppresses the delivery; nothing is
// sent. The payload carries only attributes the activation reader is granted.
type Dispatcher struct {
	Store       Store
	Client      *http.Client
	Egress      Egress
	MaxAttempts int32
	Lease       time.Duration
	// Backoff is the delay before attempt n+1 after attempt n failed transiently.
	Backoff func(attempt int32) time.Duration
}

// NewDispatcher returns a dispatcher with bounded retries and a short timeout, sending under the egress
// policy.
func NewDispatcher(s Store, egress Egress) Dispatcher {
	return Dispatcher{
		Store: s, Egress: egress, MaxAttempts: 5, Lease: time.Minute,
		Client: egress.Client(10 * time.Second),
		Backoff: func(attempt int32) time.Duration {
			return min(time.Duration(1<<min(attempt, 8))*time.Second, 5*time.Minute)
		},
	}
}

// Run dispatches due deliveries every interval until ctx ends.
func (d Dispatcher) Run(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := d.DispatchDue(ctx, 50); err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "delivery dispatch failed; retrying next interval")
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// DispatchDue dispatches up to limit due deliveries, claiming each one just before deciding and sending
// it, so a lease only ever covers one send. A delivery that cannot be decided is logged and left to its
// lease, or failed once its attempts are spent; it never blocks the others. It returns how many it claimed.
func (d Dispatcher) DispatchDue(ctx context.Context, limit int) (int, error) {
	claimed := 0
	for claimed < limit && ctx.Err() == nil {
		rows, err := db.New(d.Store.Pool).ClaimDueDeliveries(ctx, db.ClaimDueDeliveriesParams{LeaseSeconds: int32(d.Lease / time.Second), RowLimit: 1})
		if err != nil {
			return claimed, err
		}
		if len(rows) == 0 {
			break
		}
		claimed++
		if err = d.dispatch(ctx, rows[0]); err != nil {
			slog.ErrorContext(ctx, "delivery could not be dispatched", "delivery_id", rows[0].ID, "attempt", rows[0].Attempts)
		}
	}
	return claimed, nil
}

type deliveryPayload struct {
	DeliveryID      string              `json:"delivery_id"`
	ActivationID    string              `json:"activation_id"`
	Segment         string              `json:"segment"`
	SegmentRevision int32               `json:"segment_revision"`
	PurposeID       string              `json:"purpose_id"`
	AnchorProfileID string              `json:"anchor_profile_id"`
	Attributes      []deliveryAttribute `json:"attributes"`
}

type deliveryAttribute struct {
	Path   string          `json:"path"`
	Values []deliveryValue `json:"values"`
}

type deliveryValue struct {
	Value  json.RawMessage `json:"value"`
	Source string          `json:"source"`
}

// decide reads the delivery's current eligibility in one read-only snapshot. gate is empty when it may
// be sent, and the payload and target are then set.
func (d Dispatcher) decide(ctx context.Context, c db.ClaimDueDeliveriesRow) (gate string, body []byte, target string, secrets [][]byte, err error) {
	tx, err := d.Store.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return "", nil, "", nil, err
	}
	defer tx.Rollback(ctx)
	q := db.New(d.Store.Pool).WithTx(tx)
	destination, err := q.GetDestination(ctx, c.Destination)
	if err != nil {
		return "", nil, "", nil, err
	}
	if !d.Egress.Allows(destination.Url) {
		return "destination_not_allowed", nil, "", nil, nil
	}
	if secrets, err = d.Store.signingSecrets(ctx, q, destination.Name); err != nil {
		return "", nil, "", nil, err
	}
	row, err := q.GetSegmentRevision(ctx, db.GetSegmentRevisionParams{Name: c.SegmentName, Revision: c.SegmentRevision})
	if err != nil {
		return "", nil, "", nil, err
	}
	rule, err := governedRule(ctx, q, row)
	var refused *Problem
	if errors.As(err, &refused) {
		return "segment_not_governed", nil, "", nil, nil
	}
	if err != nil {
		return "", nil, "", nil, err
	}
	kratos, err := q.GetCustomerLinkForProfile(ctx, c.AnchorProfileID)
	if errors.Is(err, pgx.ErrNoRows) {
		return gateUnavailable, nil, "", nil, nil
	}
	if err != nil {
		return "", nil, "", nil, err
	}
	m, err := evaluateMember(ctx, q, rule, c.AnchorProfileID, kratos, time.Now().UTC())
	if err != nil || m.Gate != "" {
		return m.Gate, nil, "", nil, err
	}
	view, err := customerViewIn(ctx, q, c.AnchorProfileID, kratos, profiles.ReaderActivation)
	if errors.As(err, &refused) {
		return gateUnavailable, nil, "", nil, nil
	}
	if err != nil {
		return "", nil, "", nil, err
	}
	payload := deliveryPayload{DeliveryID: c.ID, ActivationID: c.ActivationID, Segment: c.SegmentName, SegmentRevision: c.SegmentRevision,
		PurposeID: c.PurposeID, AnchorProfileID: c.AnchorProfileID, Attributes: []deliveryAttribute{}}
	for _, a := range view.Attributes {
		values := make([]deliveryValue, 0, len(a.Values))
		for _, v := range a.Values {
			values = append(values, deliveryValue{Value: v.Value, Source: v.Source})
		}
		payload.Attributes = append(payload.Attributes, deliveryAttribute{Path: a.Path, Values: values})
	}
	body, err = json.Marshal(payload)
	return "", body, destination.Url, secrets, err
}

func (d Dispatcher) dispatch(ctx context.Context, c db.ClaimDueDeliveriesRow) error {
	// A delivery may make MaxAttempts after it was queued or last redriven. A claim past the bound comes
	// from a lease that expired after the last permitted attempt.
	limit := c.AttemptBase + d.MaxAttempts
	if c.Attempts > limit {
		return d.settle(ctx, c, "failed", "failed", "retries_exhausted", nil, time.Now())
	}
	gate, body, target, secrets, err := d.decide(ctx, c)
	if errors.Is(err, ErrSigningKeyUnavailable) {
		// Our own key configuration, not the destination: the delivery waits for the key, within its
		// attempts, and can be redriven once the key is back. Nothing is sent unsigned.
		if c.Attempts >= limit {
			return d.settle(ctx, c, "failed", "failed", "signing_key_unavailable", nil, time.Now())
		}
		return d.settle(ctx, c, "pending", "retry", "signing_key_unavailable", nil, time.Now().Add(d.Backoff(c.Attempts)))
	}
	if err != nil {
		if c.Attempts >= limit {
			return errors.Join(err, d.settle(ctx, c, "failed", "failed", "dispatch_error", nil, time.Now()))
		}
		return err
	}
	if gate != "" {
		state := "suppressed"
		if gate == "destination_not_allowed" {
			state = "failed"
		}
		return d.settle(ctx, c, state, state, gate, nil, time.Now())
	}
	code, sendErr := d.send(ctx, c, target, secrets, body)
	switch {
	case errors.Is(sendErr, errEgressRefused):
		return d.settle(ctx, c, "failed", "failed", "destination_address_refused", nil, time.Now())
	case sendErr == nil && code >= 200 && code < 300:
		return d.settle(ctx, c, "delivered", "delivered", "accepted", &code, time.Now())
	case sendErr == nil && code != 408 && code != 429 && code < 500:
		return d.settle(ctx, c, "failed", "failed", "rejected", &code, time.Now())
	case c.Attempts >= limit:
		return d.settle(ctx, c, "failed", "failed", "retries_exhausted", codeOrNil(code, sendErr), time.Now())
	}
	reason := "transient_status"
	if sendErr != nil {
		reason = "transport_error"
	}
	return d.settle(ctx, c, "pending", "retry", reason, codeOrNil(code, sendErr), time.Now().Add(d.Backoff(c.Attempts)))
}

func codeOrNil(code int32, err error) *int32 {
	if err != nil {
		return nil
	}
	return &code
}

// send posts the payload once. The delivery ID is the idempotency key, so a retry after a lost response
// or an expired lease is recognisable to the receiver. The signature covers the timestamp and body.
func (d Dispatcher) send(ctx context.Context, c db.ClaimDueDeliveriesRow, target string, secrets [][]byte, body []byte) (int32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", c.ID)
	req.Header.Set("CF-Delivery-Attempt", strconv.Itoa(int(c.Attempts)))
	req.Header.Set("CF-Signature", signature(secrets, time.Now(), body))
	res, err := d.Client.Do(req)
	if err != nil {
		return 0, err
	}
	res.Body.Close()
	return int32(res.StatusCode), nil
}

// settle records the attempt only while this dispatcher still holds its claim; a lost lease means
// another dispatcher owns the delivery now.
func (d Dispatcher) settle(ctx context.Context, c db.ClaimDueDeliveriesRow, state, outcome, reason string, code *int32, next time.Time) error {
	// Record what was decided or sent even if shutdown cancelled the dispatch.
	ctx = context.WithoutCancel(ctx)
	tx, err := d.Store.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	q := db.New(d.Store.Pool).WithTx(tx)
	settled, err := q.SettleDelivery(ctx, db.SettleDeliveryParams{State: state, Reason: pgtype.Text{String: reason, Valid: true},
		NextAttemptAt: next, ID: c.ID, Attempt: c.Attempts})
	if err != nil {
		return err
	}
	if settled == 0 {
		slog.WarnContext(ctx, "delivery lease lost before settling", "delivery_id", c.ID)
		return nil
	}
	status := pgtype.Int4{}
	if code != nil {
		status = pgtype.Int4{Int32: *code, Valid: true}
	}
	if err = q.InsertReceipt(ctx, db.InsertReceiptParams{DeliveryID: c.ID, Attempt: c.Attempts, Outcome: outcome, Reason: reason, StatusCode: status}); err != nil {
		return err
	}
	slog.InfoContext(ctx, "delivery settled", "delivery_id", c.ID, "attempt", c.Attempts, "outcome", outcome, "reason", reason)
	return tx.Commit(ctx)
}

// AudienceMetrics are aggregate counts for operating segments and activation. They hold no customer data.
type AudienceMetrics struct {
	Segments    int            `json:"segments"`
	Snapshots   int            `json:"snapshots"`
	Activations int            `json:"activations"`
	Deliveries  map[string]int `json:"deliveries"`
	// Reasons counts suppressed and failed deliveries by reason code, keyed "state/reason".
	Reasons map[string]int `json:"reasons"`
	// OldestPendingSeconds is how long the oldest pending delivery has waited since it was queued, last
	// attempted or redriven.
	OldestPendingSeconds int64 `json:"oldest_pending_seconds"`
}

func (s Store) AudienceMetrics(ctx context.Context) (AudienceMetrics, error) {
	var m AudienceMetrics
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return m, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	row, err := q.AudienceMetrics(ctx)
	if err != nil {
		return m, err
	}
	reasons, err := q.DeliveryReasonCounts(ctx)
	if err != nil {
		return m, err
	}
	m = AudienceMetrics{Segments: int(row.Segments), Snapshots: int(row.Snapshots), Activations: int(row.Activations),
		Deliveries:           map[string]int{"pending": int(row.Pending), "delivered": int(row.Delivered), "suppressed": int(row.Suppressed), "failed": int(row.Failed)},
		Reasons:              map[string]int{},
		OldestPendingSeconds: row.OldestPendingSeconds}
	for _, r := range reasons {
		m.Reasons[r.State+"/"+r.Reason.String] = int(r.Deliveries)
	}
	return m, nil
}
