package platform

import (
	"context"
	"errors"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
)

// ResolveCustomerProfile provisions a dedicated blank profile on first use.
// The caller must verify the active Kratos subject before calling this method.
func (s Store) ResolveCustomerProfile(ctx context.Context, kratosIdentityID string) (string, error) {
	// Read committed lets the lookup see a concurrent link committed while
	// this transaction was waiting on the subject lock.
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	q := db.New(s.Pool).WithTx(tx)
	if err := q.LockCustomerIdentity(ctx, kratosIdentityID); err != nil {
		return "", err
	}
	profileID, err := q.GetCustomerProfileID(ctx, kratosIdentityID)
	if err == nil {
		return profileID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	profileID = UUID()
	if err := q.CreateProfile(ctx, profileID); err != nil {
		return "", err
	}
	if err := q.LinkCustomerProfile(ctx, db.LinkCustomerProfileParams{
		KratosIdentityID: kratosIdentityID,
		ProfileID:        profileID,
	}); err != nil {
		return "", err
	}
	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return profileID, nil
}
