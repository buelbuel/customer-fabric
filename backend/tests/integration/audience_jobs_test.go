package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

const audienceJobsPath = "/v1/operator/audience-jobs"

func (s linkingSuite) submitJob(kind, segment, body, key string, status int) platform.AudienceJob {
	s.t.Helper()
	res := s.request("POST", segmentsPath+"/"+segment+"/"+kind+"-jobs", body, key, viewOperator, "")
	s.expect(res, status)
	var job platform.AudienceJob
	if status < 300 {
		if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
			s.t.Fatal(err)
		}
	}
	return job
}

func (s linkingSuite) job(id string) platform.AudienceJob {
	s.t.Helper()
	res := s.request("GET", audienceJobsPath+"/"+id, "", "", viewOperator, "")
	s.expect(res, 200)
	var job platform.AudienceJob
	if err := json.Unmarshal(res.Body.Bytes(), &job); err != nil {
		s.t.Fatal(err)
	}
	return job
}

// worker returns a job worker that decides 500 customers per round, so a 2,500-customer fixture spans
// several rounds inside one attempt.
func (s linkingSuite) worker() platform.AudienceWorker {
	w := platform.NewAudienceWorker(platform.Store{Pool: s.pool})
	w.Chunk = 500
	return w
}

func (s linkingSuite) step(w platform.AudienceWorker) {
	s.t.Helper()
	if worked, err := w.Step(context.Background()); !worked || err != nil {
		s.t.Fatalf("step: worked %v, %v", worked, err)
	}
}

// members pages every member of the segment's live preview.
func (s linkingSuite) members(segment string) []string {
	s.t.Helper()
	var out []string
	for cursor := ""; ; {
		query := "?limit=100"
		if cursor != "" {
			query += "&cursor=" + cursor
		}
		page, _ := s.preview(segment, query)
		for _, m := range page.Sample {
			out = append(out, m.AnchorProfileID)
		}
		if cursor = page.NextCursor; cursor == "" {
			return out
		}
	}
}

// snapshotMembers pages every member of a stored snapshot.
func (s linkingSuite) snapshotMembers(id string) []string {
	s.t.Helper()
	var out []string
	for cursor := ""; ; {
		path := "/v1/operator/segment-snapshots/" + id + "/members?limit=200"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		res := s.request("GET", path, "", "", viewOperator, "")
		s.expect(res, 200)
		var page platform.SnapshotMembers
		if err := json.Unmarshal(res.Body.Bytes(), &page); err != nil {
			s.t.Fatal(err)
		}
		out = append(out, page.Members...)
		if cursor = page.NextCursor; cursor == "" {
			return out
		}
	}
}

// withdraw records a customer's withdrawal for the purpose they granted, as committed evidence.
func (s linkingSuite) withdraw(anchor string) {
	s.t.Helper()
	if _, err := s.pool.Exec(context.Background(), `INSERT INTO consent_evidence(id, subject_id, policy_version_id, decision, actor_kind, occurred_at, proof_channel, proof_reference, idempotency_key)
		SELECT gen_random_uuid(), s.id, e.policy_version_id, 'withdraw', 'customer', now(), 'fixture', 'fixture', 'fixture-late-' || s.id
		FROM consent_subjects s JOIN consent_evidence e ON e.subject_id = s.id WHERE s.kind = 'customer' AND s.anchor_profile_id = $1 LIMIT 1`, anchor); err != nil {
		s.t.Fatal(err)
	}
}

// downgrade changes the customer's console record from gold to silver, as a source write would.
func (s linkingSuite) downgrade(anchor string) {
	s.t.Helper()
	tag, err := s.pool.Exec(context.Background(), `UPDATE profiles p SET attributes = jsonb_set(p.attributes, '{tier}', '"silver"'), version = p.version + 1
		FROM verified_associations a JOIN customer_identity_links l ON l.kratos_identity_id = a.customer_subject
		WHERE l.profile_id = $1 AND a.source = 'console' AND p.id = a.record_profile_id`, anchor)
	if err != nil || tag.RowsAffected() != 1 {
		s.t.Fatalf("downgrade: %v", err)
	}
}

// A job's snapshot is one database state. Consent withdrawn and a source value changed between
// evaluation rounds, for customers both already and not yet decided, are all absent from the result,
// exactly as if the evaluation had been instantaneous; the next job sees all of them.
func TestAudienceJobSnapshotIsOneCoherentState(t *testing.T) {
	s := setupActivation(t).linkingSuite
	f := buildAudience(s, 2500)
	before := s.members("gold")
	wantMembers, wantExcluded := f.expected()
	if len(before) != wantMembers || len(before) < 10 {
		t.Fatalf("fixture has %d members, want %d", len(before), wantMembers)
	}
	early, late, changed := before[0], before[len(before)-1], before[len(before)-2]

	s.submitJob("snapshot", "gold", "", "", 400)
	s.submitJob("snapshot", "contract_unknown", "", "job-key-coherent-0001", 404)
	s.expect(s.request("POST", segmentsPath+"/gold/snapshot-jobs", "", "job-key-coherent-0001", tokenA, ""), 401)
	job := s.submitJob("snapshot", "gold", "", "job-key-coherent-0001", 202)
	if again := s.submitJob("snapshot", "gold", "", "job-key-coherent-0001", 200); again.ID != job.ID {
		t.Fatalf("a repeated submission queued another job: %s and %s", job.ID, again.ID)
	}
	s.submitJob("activation", "gold", `{"destination":"missing_destination"}`, "job-key-coherent-0002", 422)
	if job.State != "queued" || job.Revision != 1 || job.Evaluated != 0 || job.SnapshotID != "" {
		t.Fatalf("queued job %+v", job)
	}

	w := s.worker()
	rounds := 0
	w.OnChunk = func(evaluated int) error {
		rounds++
		if evaluated != 500 {
			return nil
		}
		// One round is decided and four are not. Nothing is published, and progress is visible.
		if running := s.job(job.ID); running.State != "running" || running.Evaluated != 500 || running.SnapshotID != "" || s.count("segment_snapshots") != 0 {
			t.Errorf("mid-evaluation job %+v", running)
		}
		s.withdraw(early)
		s.withdraw(late)
		s.downgrade(changed)
		return nil
	}
	s.step(w)
	done := s.job(job.ID)
	if done.State != "completed" || done.Attempts != 1 || done.Evaluated != f.population() || done.Members != wantMembers ||
		done.Excluded != wantExcluded || done.EvaluatedAt == nil || done.SnapshotID == "" || rounds < 5 {
		t.Fatalf("completed job %+v after %d rounds", done, rounds)
	}
	if got := s.snapshotMembers(done.SnapshotID); !slices.Equal(got, before) {
		t.Fatalf("the snapshot is not the state the evaluation started from: %d members, want %d", len(got), len(before))
	}

	// The next job starts from the state after those changes and sees all three.
	next := s.submitJob("snapshot", "gold", "", "job-key-coherent-0003", 202)
	s.step(s.worker())
	after := s.job(next.ID)
	want := slices.DeleteFunc(slices.Clone(before), func(a string) bool { return a == early || a == late || a == changed })
	if got := s.snapshotMembers(after.SnapshotID); !slices.Equal(got, want) || after.Members != wantMembers-3 {
		t.Fatalf("the next snapshot has %d members, want %d", len(got), len(want))
	}
	left := []string{early, late, changed}
	slices.Sort(left)
	if c := s.changes(after.SnapshotID, done.SnapshotID); !slices.Equal(c.Left, left) || len(c.Joined) != 0 {
		t.Fatalf("changes between the two snapshots: left %v joined %v", c.Left, c.Joined)
	}
	if worked, err := s.worker().Step(context.Background()); worked || err != nil {
		t.Fatalf("a step with nothing waiting: %v %v", worked, err)
	}
	listed := s.request("GET", audienceJobsPath, "", "", viewOperator, "")
	s.expect(listed, 200)
	var page struct{ Items []platform.AudienceJob }
	if err := json.Unmarshal(listed.Body.Bytes(), &page); err != nil || len(page.Items) != 2 || page.Items[0].ID != next.ID {
		t.Fatalf("job list %s", listed.Body.String())
	}
}

// An attempt cut short publishes nothing, and the retry is one coherent evaluation of the state it
// starts from, never a continuation. Deliveries are queued once and each is decided again at dispatch.
func TestAudienceJobRetriesUnderANewSnapshotAfterACrash(t *testing.T) {
	r := newReceiver(t)
	s := setupActivation(t, r)
	buildAudience(s.linkingSuite, 2500)
	before := s.members("gold")
	gone, later := before[3], before[len(before)-4]
	s.destination("jobs", r)
	job := s.submitJob("activation", "gold", `{"destination":"jobs"}`, "job-key-crash-000001", 202)

	// The first worker stops, as on a restart, after three of its five rounds.
	ctx, stop := context.WithCancel(context.Background())
	crashing := s.worker()
	crashing.OnChunk = func(evaluated int) error {
		if evaluated == 1500 {
			stop()
		}
		return nil
	}
	if worked, err := crashing.Step(ctx); !worked || err == nil {
		t.Fatalf("an interrupted attempt: worked %v, %v", worked, err)
	}
	if interrupted := s.job(job.ID); interrupted.State != "running" || interrupted.Attempts != 1 || interrupted.ActivationID != "" ||
		s.count("activations") != 0 || s.count("activation_deliveries") != 0 {
		t.Fatalf("an interrupted attempt left something behind: %+v", interrupted)
	}

	// A customer the first attempt had already admitted withdraws before the retry.
	s.withdraw(gone)
	s.step(s.worker())
	done := s.job(job.ID)
	want := slices.DeleteFunc(slices.Clone(before), func(a string) bool { return a == gone })
	if done.State != "completed" || done.Attempts != 2 || done.Members != len(want) || s.count("activations") != 1 || s.count("activation_deliveries") != len(want) {
		t.Fatalf("retried job %+v with %d deliveries, want %d", done, s.count("activation_deliveries"), len(want))
	}

	// Queued is not permitted: a withdrawal after the job completed is suppressed at dispatch.
	s.withdraw(later)
	for s.dispatch() > 0 {
	}
	var delivered, suppressed int
	var reason string
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FILTER (WHERE state = 'delivered'), count(*) FILTER (WHERE state = 'suppressed') FROM activation_deliveries WHERE activation_id = $1`, done.ActivationID).Scan(&delivered, &suppressed)
	_ = s.pool.QueryRow(context.Background(), `SELECT reason FROM activation_deliveries WHERE activation_id = $1 AND anchor_profile_id = $2`, done.ActivationID, later).Scan(&reason)
	if delivered != len(want)-1 || suppressed != 1 || reason != "consent_not_permitted" || len(r.deliveries()) != len(want)-1 {
		t.Fatalf("dispatch: %d delivered, %d suppressed (%s), receiver got %d", delivered, suppressed, reason, len(r.deliveries()))
	}

	// An attempt that keeps failing ends the job with a reason code and publishes nothing.
	failing := s.submitJob("snapshot", "gold", "", "job-key-crash-000002", 202)
	broken := s.worker()
	broken.MaxAttempts = 2
	broken.OnChunk = func(int) error { return errors.New("synthetic failure") }
	for range 2 {
		if worked, err := broken.Step(context.Background()); !worked || err == nil {
			t.Fatalf("a failing attempt: worked %v, %v", worked, err)
		}
	}
	s.step(broken)
	if failed := s.job(failing.ID); failed.State != "failed" || failed.Reason != "retries_exhausted" || failed.SnapshotID != "" || s.count("segment_snapshots") != 0 {
		t.Fatalf("exhausted job %+v", failed)
	}
}

// Cancellation, lost governance, concurrent workers and the backlog limit.
func TestAudienceJobControls(t *testing.T) {
	s := setupActivation(t).linkingSuite
	buildAudience(s, 2500)

	// A waiting job is cancelled and never runs; a finished job cannot be cancelled.
	waiting := s.submitJob("snapshot", "gold", "", "job-key-control-0001", 202)
	cancel := func(id string, status int) {
		t.Helper()
		s.expect(s.request("POST", audienceJobsPath+"/"+id+"/cancel", "", "", viewOperator, ""), status)
	}
	s.expect(s.request("POST", audienceJobsPath+"/"+waiting.ID+"/cancel", "", "", tokenA, ""), 401)
	cancel(waiting.ID, 200)
	cancel(waiting.ID, 409)
	cancel(platform.UUID(), 404)
	if worked, _ := s.worker().Step(context.Background()); worked || s.job(waiting.ID).State != "cancelled" {
		t.Fatal("a cancelled job ran")
	}

	// A job cancelled while it is being evaluated publishes nothing, even though the evaluation finishes.
	running := s.submitJob("snapshot", "gold", "", "job-key-control-0002", 202)
	w := s.worker()
	w.OnChunk = func(evaluated int) error {
		if evaluated == 1000 {
			cancel(running.ID, 200)
		}
		return nil
	}
	s.step(w)
	if cancelled := s.job(running.ID); cancelled.State != "cancelled" || cancelled.Reason != "cancelled_by_operator" || cancelled.SnapshotID != "" || s.count("segment_snapshots") != 0 {
		t.Fatalf("a job cancelled mid-evaluation %+v", cancelled)
	}

	// Two workers racing for one job run it once.
	raced := s.submitJob("snapshot", "gold", "", "job-key-control-0003", 202)
	var wg sync.WaitGroup
	for range 2 {
		wg.Go(func() {
			if _, err := s.worker().Step(context.Background()); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	if once := s.job(raced.ID); once.State != "completed" || once.Attempts != 1 || s.count("segment_snapshots") != 1 {
		t.Fatalf("a raced job %+v with %d snapshots", once, s.count("segment_snapshots"))
	}

	// The backlog is bounded: past it a submission is refused, not queued.
	for i := range 20 {
		s.submitJob("snapshot", "gold", "", fmt.Sprintf("job-key-backlog-%04d", i), 202)
	}
	s.submitJob("snapshot", "gold", "", "job-key-backlog-full", 503)

	// A segment that lost its governance after submission ends every waiting job, with a reason code.
	s.expect(s.request("POST", "/v1/operator/attributes/tier/access", `{"readers":["operator"],"sources":["console","importer"]}`, "", viewOperator, `"1"`), 201)
	s.submitJob("snapshot", "gold", "", "job-key-control-0004", 409)
	for range 20 {
		s.step(s.worker())
	}
	var ungoverned int
	_ = s.pool.QueryRow(context.Background(), `SELECT count(*) FROM audience_jobs WHERE state = 'failed' AND reason = 'segment_not_governed'`).Scan(&ungoverned)
	if ungoverned != 20 || s.count("segment_snapshots") != 1 {
		t.Fatalf("%d jobs ended as ungoverned, %d snapshots", ungoverned, s.count("segment_snapshots"))
	}
}

// TestAudienceJobBenchmark measures snapshot and activation jobs over 100k and 250k synthetic customers,
// next to the synchronous snapshot under its default 10 s limit. It is opt-in: make audience-benchmark.
func TestAudienceJobBenchmark(t *testing.T) {
	if os.Getenv("CF_AUDIENCE_BENCHMARK") != "1" {
		t.Skip("set CF_AUDIENCE_BENCHMARK=1 to run the 100k/250k audience job benchmark")
	}
	for _, n := range []int{100_000, 250_000} {
		t.Run(fmt.Sprint(n), func(t *testing.T) {
			r := newReceiver(t)
			s := setupActivation(t, r)
			f := buildAudience(s.linkingSuite, n)
			members, _ := f.expected()
			s.destination("bench", r)
			started := time.Now()
			res := s.request("POST", segmentsPath+"/gold/snapshots", "", "", viewOperator, "")
			t.Logf("synchronous snapshot over %d customers: HTTP %d after %s", n, res.Code, time.Since(started).Round(time.Millisecond))
			for i, kind := range []string{"snapshot", "activation"} {
				body := ""
				if kind == "activation" {
					body = `{"destination":"bench"}`
				}
				job := s.submitJob(kind, "gold", body, fmt.Sprintf("job-key-bench-%d-%04d", n, i), 202)
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				started := time.Now()
				s.step(platform.NewAudienceWorker(platform.Store{Pool: s.pool}))
				elapsed := time.Since(started)
				runtime.ReadMemStats(&after)
				if done := s.job(job.ID); done.State != "completed" || done.Members != members || done.Evaluated != f.population() {
					t.Fatalf("%s job %+v", kind, done)
				}
				t.Logf("%s job over %d customers: %s, %.0f customers/s, %d members, %d MiB allocated", kind, n, elapsed.Round(time.Millisecond),
					float64(n)/elapsed.Seconds(), members, (after.TotalAlloc-before.TotalAlloc)>>20)
			}
		})
	}
}
