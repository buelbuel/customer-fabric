package platform

import (
	"context"
	"errors"
	"regexp"
	"slices"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/consent"
	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

var assessmentReference = regexp.MustCompile(`^[A-Za-z0-9._:/-]{1,128}$`)

// The change-notification contract a subscription can request. Data categories name what a notification
// may carry; only the reference envelope exists, never attribute values.
var (
	subscriptionEventTypes     = []string{"profile.created.v1", "profile.updated.v1"}
	subscriptionDataCategories = []string{"record_reference"}
)

// NotificationSubscription authorizes notifying one destination about record changes for one purpose
// (ADR 0014). Its scope never changes; a different scope is a new subscription.
type NotificationSubscription struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	PurposeID   string `json:"purpose_id"`
	// PolicyVersionID is the purpose's policy version the subscription was approved under; it authorizes
	// nothing once another version is active.
	PolicyVersionID string `json:"policy_version_id"`
	// BasisAssessment references the approved assessment behind a non-consent lawful basis.
	BasisAssessment string    `json:"basis_assessment,omitempty"`
	RecipientRole   string    `json:"recipient_role"`
	EventTypes      []string  `json:"event_types"`
	DataCategories  []string  `json:"data_categories"`
	Sources         []string  `json:"sources"`
	State           string    `json:"state"`
	CreatedBy       string    `json:"created_by"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

func subscriptionFromRow(r db.NotificationSubscription) NotificationSubscription {
	return NotificationSubscription{Name: r.Name, Destination: r.Destination, PurposeID: r.PurposeID, PolicyVersionID: r.PolicyVersionID,
		BasisAssessment: r.BasisAssessment.String, RecipientRole: r.RecipientRole,
		EventTypes: r.EventTypes, DataCategories: r.DataCategories, Sources: r.Sources, State: r.State, CreatedBy: r.CreatedBy,
		CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
}

// SubscriptionRequest is what an operator asks a new subscription to cover.
type SubscriptionRequest struct {
	Destination string `json:"destination"`
	PurposeID   string `json:"purpose_id"`
	// BasisAssessment is required when the purpose's lawful basis is not consent.
	BasisAssessment string   `json:"basis_assessment"`
	RecipientRole   string   `json:"recipient_role"`
	EventTypes      []string `json:"event_types"`
	DataCategories  []string `json:"data_categories"`
	Sources         []string `json:"sources"`
}

// CreateSubscription records a new subscription, approved under the purpose's active policy version. Its
// destination must be registered, its sources must exist, and the purpose must have an active policy
// version now, so a subscription can never be created without a documented lawful basis. A non-consent
// basis also needs the reference of the approved assessment behind it (for example a legitimate-interests
// assessment): the label alone authorizes nothing. Every notification is still authorized again before
// it is sent.
func (s Store) CreateSubscription(ctx context.Context, name string, req SubscriptionRequest, actor string) (NotificationSubscription, error) {
	invalid := func(detail string) (NotificationSubscription, error) {
		return NotificationSubscription{}, problem(422, "Invalid subscription", detail)
	}
	switch {
	case !profiles.ID.MatchString(req.PurposeID):
		return invalid("purpose_id must be a consent purpose ID.")
	case req.RecipientRole != "controller" && req.RecipientRole != "processor":
		return invalid("recipient_role must be controller or processor.")
	case len(req.EventTypes) == 0 || !subset(req.EventTypes, subscriptionEventTypes):
		return invalid("event_types must name profile.created.v1 and/or profile.updated.v1.")
	case !slices.Equal(req.DataCategories, subscriptionDataCategories):
		return invalid("data_categories must be [\"record_reference\"]: notifications carry references only.")
	case len(req.Sources) == 0 || len(req.Sources) > 20:
		return invalid("sources must name 1 to 20 sources.")
	}
	for _, source := range req.Sources {
		if !profiles.Name.MatchString(source) {
			return invalid("sources must name registered sources.")
		}
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NotificationSubscription{}, err
	}
	defer tx.Rollback(ctx)
	q := db.New(s.Pool).WithTx(tx)
	if _, err = q.GetDestination(ctx, req.Destination); errors.Is(err, pgx.ErrNoRows) {
		return invalid("destination must be a registered destination.")
	} else if err != nil {
		return NotificationSubscription{}, err
	}
	active, err := q.GetActivePolicyForPurpose(ctx, req.PurposeID)
	if errors.Is(err, pgx.ErrNoRows) {
		return invalid("The purpose needs an active policy version, which records its lawful basis.")
	}
	if err != nil {
		return NotificationSubscription{}, err
	}
	assessment := pgtype.Text{String: req.BasisAssessment, Valid: req.BasisAssessment != ""}
	switch {
	case req.BasisAssessment != "" && !assessmentReference.MatchString(req.BasisAssessment):
		return invalid("basis_assessment must be a reference of 1 to 128 letters, digits and ._:/- characters.")
	case active.LawfulBasis != string(consent.Consent) && req.BasisAssessment == "":
		return invalid("A non-consent lawful basis needs basis_assessment, the reference of its approved assessment.")
	}
	registered, err := q.ListRegisteredSources(ctx, req.Sources)
	if err != nil {
		return NotificationSubscription{}, err
	}
	if len(registered) != len(dedupe(req.Sources)) {
		return invalid("sources must name registered sources.")
	}
	row, err := q.InsertNotificationSubscription(ctx, db.InsertNotificationSubscriptionParams{ID: UUID(), Name: name, Destination: req.Destination,
		PurposeID: req.PurposeID, PolicyVersionID: active.ID, BasisAssessment: assessment, RecipientRole: req.RecipientRole, EventTypes: dedupe(req.EventTypes), DataCategories: req.DataCategories,
		Sources: dedupe(req.Sources), CreatedBy: actor})
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationSubscription{}, problem(409, "Subscription exists", "Subscriptions are immutable; register a new name.")
	}
	if err != nil {
		return NotificationSubscription{}, err
	}
	return subscriptionFromRow(row), tx.Commit(ctx)
}

// SetSubscriptionState suspends or resumes a subscription. Resuming needs the purpose's active policy
// version to still be the one the subscription was approved under.
func (s Store) SetSubscriptionState(ctx context.Context, name, state string) (NotificationSubscription, error) {
	if state != "active" && state != "suspended" {
		return NotificationSubscription{}, problem(422, "Invalid state", "state must be active or suspended.")
	}
	q := db.New(s.Pool)
	current, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return NotificationSubscription{}, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return NotificationSubscription{}, err
	}
	if state == "active" {
		active, err := q.GetActivePolicyForPurpose(ctx, current.PurposeID)
		if errors.Is(err, pgx.ErrNoRows) {
			return NotificationSubscription{}, problem(409, "No lawful basis", "The purpose has no active policy version.")
		}
		if err != nil {
			return NotificationSubscription{}, err
		}
		if active.ID != current.PolicyVersionID {
			return NotificationSubscription{}, problem(409, "Policy changed", "The purpose's active policy version is not the one this subscription was approved under; approve a new subscription.")
		}
	}
	row, err := q.SetNotificationSubscriptionState(ctx, db.SetNotificationSubscriptionStateParams{Name: name, State: state})
	return subscriptionFromRow(row), err
}

func (s Store) Subscriptions(ctx context.Context) ([]NotificationSubscription, error) {
	rows, err := db.New(s.Pool).ListNotificationSubscriptions(ctx)
	out := make([]NotificationSubscription, 0, len(rows))
	for _, r := range rows {
		out = append(out, subscriptionFromRow(r))
	}
	return out, err
}

// NotificationDelivery is the recorded outcome of one event for one subscription: identifiers and codes.
type NotificationDelivery struct {
	EventID        string    `json:"event_id"`
	ProfileID      string    `json:"profile_id"`
	ProfileVersion int64     `json:"profile_version"`
	Outcome        string    `json:"outcome"`
	Reason         string    `json:"reason"`
	Attempts       int32     `json:"attempts"`
	StatusCode     *int32    `json:"status_code,omitempty"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// SubscriptionDeliveries lists a subscription's 100 most recent outcomes.
func (s Store) SubscriptionDeliveries(ctx context.Context, name string) ([]NotificationDelivery, error) {
	q := db.New(s.Pool)
	subscription, err := q.GetNotificationSubscriptionByName(ctx, name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, problem(404, "Not found", "Unknown subscription.")
	}
	if err != nil {
		return nil, err
	}
	rows, err := q.ListNotificationDeliveries(ctx, subscription.ID)
	out := make([]NotificationDelivery, 0, len(rows))
	for _, r := range rows {
		d := NotificationDelivery{EventID: r.EventID, ProfileID: r.ProfileID, ProfileVersion: r.ProfileVersion, Outcome: r.Outcome,
			Reason: r.Reason, Attempts: r.Attempts, RecordedAt: r.RecordedAt}
		if r.StatusCode.Valid {
			d.StatusCode = &r.StatusCode.Int32
		}
		out = append(out, d)
	}
	return out, err
}

func subset(items, allowed []string) bool {
	for _, item := range items {
		if !slices.Contains(allowed, item) {
			return false
		}
	}
	return true
}

func dedupe(items []string) []string {
	out := slices.Clone(items)
	slices.Sort(out)
	return slices.Compact(out)
}
