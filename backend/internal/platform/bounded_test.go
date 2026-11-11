package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Population evaluations past the concurrency limit or their time limit are refused as 503, never queued
// without bound or returned partial.
func TestBoundedRefusesOverloadAndTimeouts(t *testing.T) {
	for range cap(evaluationSlots) {
		evaluationSlots <- struct{}{}
	}
	ran := false
	_, err := bounded(t.Context(), Store{}, func(context.Context) (int, error) { ran = true; return 1, nil })
	var p *Problem
	if !errors.As(err, &p) || p.Status != 503 || ran {
		t.Fatalf("a full limit must refuse without evaluating: %v ran=%v", err, ran)
	}
	for range cap(evaluationSlots) {
		<-evaluationSlots
	}
	_, err = bounded(t.Context(), Store{EvaluationTimeout: time.Millisecond}, func(ctx context.Context) (int, error) {
		<-ctx.Done()
		return 0, ctx.Err()
	})
	if !errors.As(err, &p) || p.Status != 503 || len(evaluationSlots) != 0 {
		t.Fatalf("a timeout must be refused and release its slot: %v", err)
	}
	if n, err := bounded(t.Context(), Store{}, func(context.Context) (int, error) { return 7, nil }); n != 7 || err != nil {
		t.Fatalf("an evaluation within limits: %d %v", n, err)
	}
}
