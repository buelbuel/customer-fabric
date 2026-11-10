package integration

import (
	"fmt"
	"os"
	"slices"
	"testing"
	"time"
)

// TestImportBenchmark imports 100,000 synthetic rows in 1,000-row batches and reports throughput and
// per-batch latency, so a slowdown as tables grow is visible. Opt in with CF_IMPORT_BENCHMARK=1.
func TestImportBenchmark(t *testing.T) {
	if os.Getenv("CF_IMPORT_BENCHMARK") != "1" {
		t.Skip("set CF_IMPORT_BENCHMARK=1 to run the 100k import benchmark")
	}
	s := setupLinking(t)
	s.expect(s.request("POST", "/v1/operator/attributes/tier", `{"type":"enum","values":["gold","silver"]}`, "", viewOperator, ""), 201)
	s.expect(s.request("POST", "/v1/operator/attributes/rank", `{"type":"integer"}`, "", viewOperator, ""), 201)
	imp := s.startImport(tokenA, `{"max_errors":0}`)
	const total, size = 100_000, 1_000
	var batches []time.Duration
	started := time.Now()
	for from := 1; from <= total; from += size {
		rows := make([]string, 0, size)
		for i := from; i < from+size; i++ {
			rows = append(rows, row(fmt.Sprintf("bench-%06d", i), fmt.Sprintf(`{"tier":"gold","rank":%d,"display_name":"Synthetic Kunde %d"}`, i, i)))
		}
		began := time.Now()
		s.expect(s.batch(tokenA, imp.ID, int64(from), rows...), 200)
		batches = append(batches, time.Since(began))
	}
	elapsed := time.Since(started)
	sorted := slices.Clone(batches)
	slices.Sort(sorted)
	first, last := mean(batches[:10]), mean(batches[len(batches)-10:])
	t.Logf("%d rows in %s (%.0f rows/s); batch p50 %s p95 %s max %s; first 10 batches %s, last 10 %s",
		total, elapsed.Round(time.Millisecond), float64(total)/elapsed.Seconds(), sorted[len(sorted)/2], sorted[len(sorted)*95/100], sorted[len(sorted)-1], first, last)
	if done := progress(t, s.request("GET", "/v1/imports/"+imp.ID, "", "", tokenA, "")); done.Created != total {
		t.Fatalf("imported %+v", done)
	}
}

func mean(ds []time.Duration) time.Duration {
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}
