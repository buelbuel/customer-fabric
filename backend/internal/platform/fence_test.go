package platform

import (
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// Every limit is finite by construction: unset ones take defaults, and none can be zero, negative or
// long enough to be no bound.
func TestFenceLimitsAreAlwaysFinite(t *testing.T) {
	if err := (FenceLimits{}).WithDefaults().Validate(); err != nil {
		t.Fatal(err)
	}
	set := FenceLimits{ChangeWait: time.Second}.WithDefaults()
	if set.ChangeWait != time.Second || set.WriteWait != DefaultFenceLimits().WriteWait || set.TransactionTimeout != DefaultFenceLimits().TransactionTimeout {
		t.Fatalf("defaults %+v", set)
	}
	for _, bad := range []FenceLimits{
		{ChangeWait: -time.Second, WriteWait: time.Second, TransactionTimeout: time.Second},
		{ChangeWait: time.Second, WriteWait: time.Millisecond, TransactionTimeout: time.Second},
		{ChangeWait: time.Second, WriteWait: time.Second, TransactionTimeout: 24 * time.Hour},
	} {
		if bad.Validate() == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if got := (FenceLimits{}).ms(1500 * time.Microsecond); got != 2 {
		t.Fatalf("a limit rounds up to the millisecond, got %d", got)
	}
}

// A bound the database enforced is a retriable 503; nothing else is turned into one.
func TestBoundedTransactionsFailAsRetriable(t *testing.T) {
	for code, title := range map[string]string{sqlLockNotAvailable: "Temporarily busy", sqlTransactionTimedOut: "Transaction timed out"} {
		p := busyProblem(fmt.Errorf("write: %w", &pgconn.PgError{Code: code, Message: "details that must not reach a client"}))
		if p == nil || p.Status != 503 || p.Title != title || p.RetryAfter == 0 {
			t.Fatalf("%s: %+v", code, p)
		}
		w := httptest.NewRecorder()
		fail(w, fmt.Errorf("write: %w", &pgconn.PgError{Code: code, Message: "details that must not reach a client"}))
		if w.Code != 503 || w.Header().Get("Retry-After") != "1" || w.Body.String() == "" || strings.Contains(w.Body.String(), "details") {
			t.Fatalf("%s: %d %v %s", code, w.Code, w.Header(), w.Body.String())
		}
	}
	for _, other := range []error{errors.New("boom"), &pgconn.PgError{Code: "23505"}} {
		if busyProblem(other) != nil {
			t.Fatalf("%v became a 503", other)
		}
	}
	w := httptest.NewRecorder()
	fail(w, errors.New("boom"))
	if w.Code != 500 || w.Header().Get("Retry-After") != "" {
		t.Fatalf("%d %v", w.Code, w.Header())
	}
}

// The monitor counts a wait once, by its outcome and length, and a nil monitor measures nothing.
func TestFenceMonitorMeasuresWaits(t *testing.T) {
	var none *FenceMonitor
	none.begin(sideWrites)(nil)
	if r := none.Report(DefaultFenceLimits()); r.Writes.Completed != 0 || r.Limits.TransactionTimeoutMs != 30000 {
		t.Fatalf("%+v", r)
	}

	m := NewFenceMonitor()
	m.begin(sideWrites)(nil)
	end := m.begin(sideWrites)
	if got := m.Report(DefaultFenceLimits()).Writes.Waiting; got != 1 {
		t.Fatalf("waiting %d", got)
	}
	time.Sleep(80 * time.Millisecond)
	end(nil)
	m.begin(sideChanges)(&pgconn.PgError{Code: sqlLockNotAvailable})
	m.begin(sideChanges)(errors.New("cancelled"))
	r := m.Report(DefaultFenceLimits())
	if w := r.Writes; w.Waiting != 0 || w.Completed != 2 || w.Blocked != 1 || w.TimedOut != 0 || w.WaitMsMax < 80 || w.Histogram.UpTo10 != 1 || w.Histogram.UpTo100 != 1 {
		t.Fatalf("writes %+v", w)
	}
	if c := r.Changes; c.Waiting != 0 || c.Completed != 0 || c.TimedOut != 1 || c.Histogram.UpTo10 != 2 {
		t.Fatalf("changes %+v", c)
	}
	if got := (Store{Fence: FenceLimits{WriteWait: -time.Second}}).fenceLimits(); got != DefaultFenceLimits() {
		t.Fatalf("an invalid limit was used: %+v", got)
	}
	if got := (Store{Fence: FenceLimits{ChangeWait: time.Second}}).fenceLimits(); got.ChangeWait != time.Second || got.WriteWait != DefaultFenceLimits().WriteWait {
		t.Fatalf("%+v", got)
	}
	if bucketOf(10*time.Second) != 3 || bucketOf(10*time.Second+1) != 4 {
		t.Fatal("bucket bounds are inclusive")
	}
}
