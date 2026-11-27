package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
)

// Opt in to measure the write path and adoption query on synthetic data.
func TestCollectionPostgresBenchmark(t *testing.T) {
	if os.Getenv("CF_COLLECTION_BENCHMARK") != "1" {
		t.Skip("set CF_COLLECTION_BENCHMARK=1 to measure collections")
	}
	s := setupLinking(t)
	s.expect(s.request("POST", "/v1/operator/attributes/interests", `{"type":"string","max_items":5}`, "", viewOperator, ""), 201)
	imp := s.startImport(tokenA, `{"max_errors":0}`)
	const total = 1000
	rows := make([]string, total)
	for i := range rows {
		rows[i] = row(fmt.Sprintf("collection-bench-%d", i), `{"interests":["travel","音楽","café"],"legacy":"gold"}`)
	}
	start := time.Now()
	p := progress(t, s.batch(tokenA, imp.ID, 1, rows...))
	writeTime := time.Since(start)
	if p.Created != total {
		t.Fatalf("import progress %+v", p)
	}
	ctx := context.Background()
	q := db.New(s.pool)
	start = time.Now()
	values, err := q.ListStoredAttributeValues(ctx, db.ListStoredAttributeValuesParams{Path: "interests", RowLimit: 10001})
	readTime := time.Since(start)
	if err != nil || len(values) != total {
		t.Fatalf("adoption query: %d values, %v", len(values), err)
	}
	t.Logf("%d collection rows: import %s (%.0f rows/s), adoption query %s", total,
		writeTime.Round(time.Millisecond), float64(total)/writeTime.Seconds(), readTime.Round(time.Microsecond))
}
