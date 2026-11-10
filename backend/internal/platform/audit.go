package platform

import (
	"context"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
)

// OperatorAuditEntry is one administrative write: who, what and which resource, never the payload.
type OperatorAuditEntry struct {
	ID       string    `json:"id"`
	Actor    string    `json:"actor"`
	Method   string    `json:"method"`
	Resource string    `json:"resource"`
	Status   int32     `json:"status"`
	At       time.Time `json:"at"`
}

func (s Store) RecordOperatorAudit(ctx context.Context, actor, method, resource string, status int) error {
	return db.New(s.Pool).InsertOperatorAudit(ctx, db.InsertOperatorAuditParams{ID: UUID(), Actor: actor, Method: method, Resource: resource, Status: int32(status)})
}

// OperatorAudit lists the 200 most recent administrative writes, newest first.
func (s Store) OperatorAudit(ctx context.Context) ([]OperatorAuditEntry, error) {
	rows, err := db.New(s.Pool).ListOperatorAudit(ctx)
	out := make([]OperatorAuditEntry, 0, len(rows))
	for _, r := range rows {
		out = append(out, OperatorAuditEntry{ID: r.ID, Actor: r.Actor, Method: r.Method, Resource: r.Resource, Status: r.Status, At: r.At})
	}
	return out, err
}
