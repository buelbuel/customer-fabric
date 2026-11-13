package platform

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

type linkRecord struct {
	Source, Namespace, Value string
}

type issuedLinkGrant struct {
	ID        string    `json:"id"`
	Grant     string    `json:"grant"`
	ExpiresAt time.Time `json:"expires_at"`
}

type verifiedAssociation struct {
	ID       string    `json:"id"`
	Source   string    `json:"source"`
	LinkedAt time.Time `json:"linked_at"`
}

func unusableGrant() *Problem {
	return problem(409, "Grant unavailable", "The grant cannot be redeemed.")
}

func associationNotFound() *Problem {
	return problem(404, "Not found", "The association is unavailable.")
}

func (s Store) issueLinkGrant(ctx context.Context, target linkRecord, subject, proofReference string) (issuedLinkGrant, error) {
	var grant issuedLinkGrant
	var secret [32]byte
	rand.Read(secret[:])
	hash := sha256.Sum256(secret[:])
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		epoch, err := q.GetLinkRecordEpoch(ctx, db.GetLinkRecordEpochParams{
			Source: target.Source, Namespace: target.Namespace, Value: target.Value,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "The source record is unavailable.")
		}
		if err != nil {
			return err
		}
		if err := q.LockLinkRecord(ctx, epoch); err != nil {
			return err
		}
		recordProfile, err := q.GetLinkRecordProfile(ctx, db.GetLinkRecordProfileParams{RecordEpoch: epoch, Source: target.Source})
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "The source record is unavailable.")
		}
		if err != nil {
			return err
		}
		active, err := q.HasActiveAssociation(ctx, epoch)
		if err != nil {
			return err
		}
		if active {
			return problem(409, "Grant unavailable", "A grant cannot be issued for this request.")
		}
		id := UUID()
		expires, err := q.CreateLinkingGrant(ctx, db.CreateLinkingGrantParams{
			ID: id, SecretHash: hash[:], Source: target.Source,
			RecordProfileID: recordProfile, RecordEpoch: epoch,
			CustomerSubject: subject, ProofReference: proofReference,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(409, "Grant unavailable", "A grant cannot be issued for this request.")
		}
		if err != nil {
			return err
		}
		grant = issuedLinkGrant{ID: id, Grant: base64.RawURLEncoding.EncodeToString(secret[:]), ExpiresAt: expires}
		return nil
	})
	return grant, err
}

func (s Store) redeemLinkGrant(ctx context.Context, subject string, hash [32]byte, issuerEnabled func(string) bool) (verifiedAssociation, error) {
	var association verifiedAssociation
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		grant, err := q.GetLinkingGrantByHash(ctx, db.GetLinkingGrantByHashParams{SecretHash: hash[:], CustomerSubject: subject})
		if errors.Is(err, pgx.ErrNoRows) {
			return unusableGrant()
		}
		if err != nil {
			return err
		}
		if !issuerEnabled(grant.Source) {
			return unusableGrant()
		}
		// Identity before record, as in AppendCustomerDecision, so a new link cannot slip past a grant's check.
		if err := q.LockCustomerIdentity(ctx, subject); err != nil {
			return err
		}
		if err := q.LockLinkRecord(ctx, grant.RecordEpoch); err != nil {
			return err
		}
		recordProfile, err := q.GetLinkRecordProfile(ctx, db.GetLinkRecordProfileParams{RecordEpoch: grant.RecordEpoch, Source: grant.Source})
		if errors.Is(err, pgx.ErrNoRows) {
			return unusableGrant()
		}
		if err != nil {
			return err
		}
		if recordProfile != grant.RecordProfileID {
			return unusableGrant()
		}
		consumed, err := q.ConsumeLinkingGrant(ctx, grant.ID)
		if err != nil {
			return err
		}
		if consumed != 1 {
			return unusableGrant()
		}
		if _, err = q.GetCustomerProfileID(ctx, subject); errors.Is(err, pgx.ErrNoRows) {
			return problem(409, "Profile required", "Provision the customer profile before redeeming a grant.")
		} else if err != nil {
			return err
		}
		row, err := q.CreateVerifiedAssociation(ctx, db.CreateVerifiedAssociationParams{
			ID: UUID(), GrantID: grant.ID, CustomerSubject: subject, Source: grant.Source,
			RecordProfileID: grant.RecordProfileID,
			RecordEpoch:     grant.RecordEpoch,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return unusableGrant()
		}
		if err != nil {
			return err
		}
		// The record now follows the customer's decisions (ADR 0014 withdrawal notices).
		if err := q.QueueReviewForRecordEpoch(ctx, grant.RecordEpoch); err != nil {
			return err
		}
		// Any other grant for this target predates the new link. It must not
		// become redeemable when this association is eventually revoked.
		if err := q.CancelPendingLinkingGrants(ctx, grant.RecordEpoch); err != nil {
			return err
		}
		association = verifiedAssociation(row)
		return nil
	})
	return association, err
}

func (s Store) revokeLinkGrant(ctx context.Context, source, id string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		epoch, err := q.GetLinkingGrantByIssuer(ctx, db.GetLinkingGrantByIssuerParams{ID: id, Source: source})
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "The grant is unavailable.")
		}
		if err != nil {
			return err
		}
		if err := q.LockLinkRecord(ctx, epoch); err != nil {
			return err
		}
		if err := q.RevokeLinkingGrant(ctx, id); err != nil {
			return err
		}
		revoked, err := q.RevokeAssociationByIssuer(ctx, id)
		if err != nil || revoked == 0 {
			return err
		}
		// A changed association changes which subject decides the record (ADR 0014 withdrawal notices).
		if err := q.QueueReviewForRecordEpoch(ctx, epoch); err != nil {
			return err
		}
		return q.CancelPendingLinkingGrants(ctx, epoch)
	})
}

func (s Store) unlinkAssociation(ctx context.Context, subject, id string) error {
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(tx)
		epoch, err := q.GetAssociationTarget(ctx, db.GetAssociationTargetParams{ID: id, CustomerSubject: subject})
		if errors.Is(err, pgx.ErrNoRows) {
			return associationNotFound()
		}
		if err != nil {
			return err
		}
		if err := q.LockLinkRecord(ctx, epoch); err != nil {
			return err
		}
		revoked, err := q.RevokeAssociationByCustomer(ctx, id)
		if err != nil || revoked == 0 {
			return err
		}
		// A changed association changes which subject decides the record (ADR 0014 withdrawal notices).
		if err := q.QueueReviewForRecordEpoch(ctx, epoch); err != nil {
			return err
		}
		return q.CancelPendingLinkingGrants(ctx, epoch)
	})
}
