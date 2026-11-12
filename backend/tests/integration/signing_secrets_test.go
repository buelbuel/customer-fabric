package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func newKeys(t *testing.T, ids ...string) (platform.SealingKeys, []string) {
	t.Helper()
	var pairs []string
	for _, id := range ids {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			t.Fatal(err)
		}
		pairs = append(pairs, id+":"+hex.EncodeToString(key))
	}
	keys, err := platform.ParseSealingKeys(strings.Join(pairs, ","))
	if err != nil {
		t.Fatal(err)
	}
	return keys, pairs
}

func parseKeys(t *testing.T, pairs ...string) platform.SealingKeys {
	t.Helper()
	keys, err := platform.ParseSealingKeys(strings.Join(pairs, ","))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// secretSuite has destinations registered before secrets were sealed, each subscribed to console changes
// and receiving on its own receiver, as a recipient configured with that secret would.
type secretSuite struct {
	activationSuite
	purpose string
	record  string
	// hooks maps a destination to its receiver, legacy to its plaintext secret, subs to its subscription.
	hooks map[string]*receiver
	subs  map[string]string
}

func setupSecrets(t *testing.T, legacy ...string) (secretSuite, map[string]string) {
	t.Helper()
	receivers := map[string]*receiver{}
	var all []*receiver
	for _, name := range append(append([]string{}, legacy...), "fresh") {
		receivers[name] = newReceiver(t)
		all = append(all, receivers[name])
	}
	s := secretSuite{activationSuite: setupActivation(t, all...), hooks: receivers, subs: map[string]string{}}
	secrets := map[string]string{}
	for _, name := range legacy {
		// A destination registered by the release before sealing: its secret is in the clear.
		secret := make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			t.Fatal(err)
		}
		if _, err := s.pool.Exec(context.Background(), `INSERT INTO destinations (name, url, signing_secret) VALUES ($1, $2, $3)`,
			name, receivers[name].URL+"/hook", secret); err != nil {
			t.Fatal(err)
		}
		secrets[name] = hex.EncodeToString(secret)
	}
	secrets["fresh"] = s.destination("fresh", receivers["fresh"])
	s.purpose, _ = s.purposeWithBasis("crm_sync", "legitimate_interests")
	for name := range receivers {
		s.subs[name] = s.subscribe("sync_"+name, subscriptionBody(name, s.purpose, "console"))
	}
	s.record = s.viewRecord(tokenA, "secret-record", `{"tier":"gold"}`)
	return s, secrets
}

// notify sends a change to one destination with store and returns the error, if any.
func (s secretSuite) notify(store platform.Store, destination string) (platform.NotificationResult, error) {
	return platform.NewNotifier(store, s.config.Egress()).Notify(context.Background(), changeOf(s.record, 1), s.subs[destination])
}

// lastSignedBy reports whether the last send to destination verifies under secret.
func (s secretSuite) lastSignedBy(destination, secret string) bool {
	got := s.hooks[destination].deliveries()
	return len(got) > 0 && signedBy(got[len(got)-1].signature, got[len(got)-1].body, secret)
}

func (s secretSuite) expectSent(store platform.Store, destination string, secrets ...string) {
	s.t.Helper()
	before := len(s.hooks[destination].deliveries())
	got, err := s.notify(store, destination)
	if err != nil || got.Outcome != platform.NotificationDelivered || len(s.hooks[destination].deliveries()) != before+1 {
		s.t.Fatalf("%s: %+v %v", destination, got, err)
	}
	for _, secret := range secrets {
		if !s.lastSignedBy(destination, secret) {
			s.t.Fatalf("%s: the send does not verify under an expected secret", destination)
		}
	}
}

func (s secretSuite) expectHeld(store platform.Store, destination string) {
	s.t.Helper()
	before := len(s.hooks[destination].deliveries())
	if _, err := s.notify(store, destination); !errors.Is(err, platform.ErrSigningKeyUnavailable) || len(s.hooks[destination].deliveries()) != before {
		s.t.Fatalf("%s: want held for the key, got %v", destination, err)
	}
}

type storedSecret struct {
	plaintext []byte
	sealed    [][]byte
	keys      []string
}

func (s secretSuite) stored(destination string) storedSecret {
	s.t.Helper()
	var st storedSecret
	ctx := context.Background()
	if err := s.pool.QueryRow(ctx, `SELECT signing_secret FROM destinations WHERE name = $1`, destination).Scan(&st.plaintext); err != nil {
		s.t.Fatal(err)
	}
	rows, err := s.pool.Query(ctx, `SELECT sealed, coalesce(key_id, '') FROM destination_secrets WHERE destination = $1 ORDER BY version`, destination)
	if err != nil {
		s.t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var sealed []byte
		var key string
		if err := rows.Scan(&sealed, &key); err != nil {
			s.t.Fatal(err)
		}
		st.sealed, st.keys = append(st.sealed, sealed), append(st.keys, key)
	}
	return st
}

func (s secretSuite) status(store platform.Store) platform.SecretStatus {
	s.t.Helper()
	st, err := store.SecretStatus(context.Background())
	if err != nil {
		s.t.Fatal(err)
	}
	return st
}

// The upgrade seals existing secrets as the same values: recipients keep verifying every send with the
// secret they already have, before, during (with some sealed and some not, or interrupted) and after
// the migration, and the plaintext is cleared only by finalize after it verifies again.
func TestPlaintextSecretsMigrateWithoutBreakingRecipients(t *testing.T) {
	s, secrets := setupSecrets(t, "legacy_a", "legacy_b")
	ctx := context.Background()
	// Before any sealing, legacy destinations sign with their plaintext, even without keys.
	unkeyed := platform.Store{Pool: s.pool}
	s.expectSent(unkeyed, "legacy_a", secrets["legacy_a"])
	if st := s.status(s.store); len(st.Plaintext) != 2 || len(st.Sealed) != 1 || st.MigrationComplete {
		t.Fatalf("status %+v", st)
	}

	// An interrupted run changes nothing.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.store.SealPlaintextSecrets(cancelled, 100); err == nil {
		t.Fatal("a cancelled run reported success")
	}
	if st := s.stored("legacy_a"); len(st.sealed) != 0 || !bytes.Equal(st.plaintext, mustHex(t, secrets["legacy_a"])) {
		t.Fatalf("an interrupted run changed legacy_a: %+v", st)
	}
	// A partial run leaves a mix; every destination still signs with the secret its recipient has.
	if n, err := s.store.SealPlaintextSecrets(ctx, 1); err != nil || n != 1 {
		t.Fatalf("sealed %d %v", n, err)
	}
	s.expectSent(s.store, "legacy_a", secrets["legacy_a"])
	s.expectSent(s.store, "legacy_b", secrets["legacy_b"])
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy_a", "legacy_b"} {
		st := s.stored(name)
		if len(st.sealed) != 1 || len(st.sealed[0]) != 60 || st.keys[0] != "test-current" || !bytes.Equal(st.plaintext, mustHex(t, secrets[name])) {
			t.Fatalf("%s after sealing: %+v", name, st)
		}
		if bytes.Contains(st.sealed[0], st.plaintext) {
			t.Fatalf("%s: the sealed value contains the secret", name)
		}
		s.expectSent(s.store, name, secrets[name])
	}
	if st := s.status(s.store); len(st.Unfinalized) != 2 || len(st.Plaintext) != 0 || st.MigrationComplete || !st.SigningAvailable {
		t.Fatalf("status %+v", st)
	}

	// Finalize verifies again and only then clears the plaintext; recipients see no change.
	report, err := s.store.FinalizeSecretMigration(ctx)
	if err != nil || len(report.Changed) != 2 || len(report.Refused) != 0 {
		t.Fatalf("finalize %+v %v", report, err)
	}
	for _, name := range []string{"legacy_a", "legacy_b", "fresh"} {
		if st := s.stored(name); st.plaintext != nil {
			t.Fatalf("%s kept its plaintext", name)
		}
		s.expectSent(s.store, name, secrets[name])
	}
	if st := s.status(s.store); !st.MigrationComplete || len(st.Sealed) != 3 {
		t.Fatalf("status %+v", st)
	}
	// Repeating is harmless.
	if report, err = s.store.FinalizeSecretMigration(ctx); err != nil || len(report.Changed) != 0 {
		t.Fatalf("second finalize %+v %v", report, err)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Sends racing the migration and its finalization are always signed with the secret the recipient has.
func TestConcurrentSendsDuringMigrationVerify(t *testing.T) {
	s, secrets := setupSecrets(t, "legacy_a")
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 200)
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got, err := s.notify(s.store, "legacy_a"); err != nil || got.Outcome != platform.NotificationDelivered {
					errs <- fmt.Errorf("%+v %v", got, err)
					return
				}
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	if report, err := s.store.FinalizeSecretMigration(ctx); err != nil || len(report.Changed) != 1 {
		t.Fatalf("finalize %+v %v", report, err)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got := s.hooks["legacy_a"].deliveries()
	if len(got) < 3 {
		t.Fatalf("only %d sends", len(got))
	}
	for _, d := range got {
		if !signedBy(d.signature, d.body, secrets["legacy_a"]) {
			t.Fatalf("a send during the migration does not verify: %q", d.signature)
		}
	}
}

// A missing or wrong key holds sends to sealed destinations, and changes, regenerates or removes
// nothing; the plaintext of a destination not yet finalized is kept. Configuring the right key resumes.
func TestMissingOrWrongKeyFailsClosedWithoutDestroyingSecrets(t *testing.T) {
	s, secrets := setupSecrets(t, "legacy_a", "legacy_b")
	ctx := context.Background()
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE destinations SET signing_secret = NULL WHERE name = 'legacy_b'`); err != nil {
		t.Fatal(err)
	}
	before := map[string]storedSecret{}
	for _, name := range []string{"legacy_a", "legacy_b", "fresh"} {
		before[name] = s.stored(name)
	}

	wrong, _ := newKeys(t, "test-current")
	for _, store := range []platform.Store{{Pool: s.pool}, {Pool: s.pool, Keys: wrong}} {
		for _, name := range []string{"legacy_a", "legacy_b", "fresh"} {
			// legacy_a still has its plaintext, but once sealed only the sealed secret counts.
			s.expectHeld(store, name)
		}
		if st := s.status(store); st.SigningAvailable || len(st.Unavailable) != 3 {
			t.Fatalf("status %+v", st)
		}
		if err := (platform.Housekeeper{Store: store}).Step(ctx); err != nil {
			t.Fatal(err)
		}
		if report, err := store.FinalizeSecretMigration(ctx); err != nil || len(report.Changed) != 0 || len(report.Refused) != 1 {
			t.Fatalf("finalize with an unusable key %+v %v", report, err)
		}
		if report, err := store.RollbackSecretMigration(ctx); err != nil || len(report.Changed) != 0 || len(report.Refused) != 2 {
			t.Fatalf("rollback with an unusable key %+v %v", report, err)
		}
		api, err := platform.NewAPI(store, s.config)
		if err != nil {
			t.Fatal(err)
		}
		s.h = api
		s.expect(s.request("POST", "/v1/operator/destinations/fresh/signing-secret", `{"overlap_seconds":60}`, "", viewOperator, ""), 503)
		s.expect(s.request("POST", "/v1/operator/destinations/another", fmt.Sprintf(`{"url":%q}`, s.hooks["fresh"].URL+"/hook"), "", viewOperator, ""), 503)
		for name, st := range before {
			after := s.stored(name)
			if !bytes.Equal(after.plaintext, st.plaintext) || len(after.sealed) != len(st.sealed) || !bytes.Equal(after.sealed[0], st.sealed[0]) || after.keys[0] != st.keys[0] {
				t.Fatalf("%s changed under an unusable key", name)
			}
		}
	}
	// The activation dispatcher holds too, and delivers once the key is back.
	s.h, _ = platform.NewAPI(s.store, s.config)
	for _, name := range []string{"legacy_a", "legacy_b", "fresh"} {
		s.expectSent(s.store, name, secrets[name])
	}
}

// Rotating the deployment key reseals every secret under the new key in the background, without any
// recipient noticing; the old key can then be removed.
func TestSealingKeyRotation(t *testing.T) {
	s, secrets := setupSecrets(t, "legacy_a")
	ctx := context.Background()
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	_, next := newKeys(t, "test-next")
	rotated := platform.Store{Pool: s.pool, Keys: parseKeys(t, append(next, testKeyPair)...)}
	if st := s.status(rotated); st.RotationComplete || len(st.Stale) != 2 || !st.SigningAvailable {
		t.Fatalf("status before resealing %+v", st)
	}
	// Both keys open during the rotation.
	s.expectSent(rotated, "legacy_a", secrets["legacy_a"])
	if err := (platform.Housekeeper{Store: rotated}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.status(rotated); !st.RotationComplete {
		t.Fatalf("status after resealing %+v", st)
	}
	onlyNext := platform.Store{Pool: s.pool, Keys: parseKeys(t, next...)}
	for _, name := range []string{"legacy_a", "fresh"} {
		if st := s.stored(name); st.keys[0] != "test-next" {
			t.Fatalf("%s still sealed under %s", name, st.keys[0])
		}
		s.expectSent(onlyNext, name, secrets[name])
	}
	// The old key alone no longer opens anything.
	s.expectHeld(s.store, "fresh")
}

// Rotating a destination's secret keeps the previous one signing for the overlap, so a recipient can
// switch without rejecting sends; a zero overlap ends it at once. Erased secrets are gone.
func TestDestinationSecretRotation(t *testing.T) {
	s, secrets := setupSecrets(t, "legacy_a")
	ctx := context.Background()
	rotate := func(name, body string, code int) platform.SecretRotation {
		t.Helper()
		res := s.request("POST", "/v1/operator/destinations/"+name+"/signing-secret", body, "", viewOperator, "")
		s.expect(res, code)
		var r platform.SecretRotation
		if code == 201 {
			decodeInto(t, res.Body.Bytes(), &r)
		}
		return r
	}
	rotate("missing", `{"overlap_seconds":60}`, 404)
	rotate("fresh", `{"overlap_seconds":604801}`, 422)
	rotate("fresh", `{}`, 422)
	s.expect(s.request("POST", "/v1/operator/destinations/fresh/signing-secret", `{"overlap_seconds":60}`, "", tokenA, ""), 401)

	// A plaintext destination is sealed first, so its current secret keeps signing during the overlap.
	r := rotate("legacy_a", `{"overlap_seconds":3600}`, 201)
	if r.Version != 2 || r.PreviousValidUntil == nil || len(r.SigningSecret) != 64 {
		t.Fatalf("rotation %+v", r)
	}
	if st := s.stored("legacy_a"); st.plaintext != nil || len(st.sealed) != 2 {
		t.Fatalf("legacy_a after rotation %+v", st)
	}
	s.expectSent(s.store, "legacy_a", secrets["legacy_a"], r.SigningSecret)

	r2 := rotate("legacy_a", `{"overlap_seconds":0}`, 201)
	if r2.Version != 3 || r2.PreviousValidUntil != nil {
		t.Fatalf("rotation %+v", r2)
	}
	s.expectSent(s.store, "legacy_a", r2.SigningSecret)
	if s.lastSignedBy("legacy_a", r.SigningSecret) || s.lastSignedBy("legacy_a", secrets["legacy_a"]) {
		t.Fatal("a secret rotated out without overlap still signs")
	}
	if st := s.stored("legacy_a"); st.sealed[0] != nil || st.sealed[1] != nil || st.sealed[2] == nil {
		t.Fatalf("rotated-out secrets were not erased: %+v", st)
	}

	// An overlap that ends is erased by the housekeeper.
	r3 := rotate("fresh", `{"overlap_seconds":60}`, 201)
	s.expectSent(s.store, "fresh", secrets["fresh"], r3.SigningSecret)
	if _, err := s.pool.Exec(ctx, `UPDATE destination_secrets SET retires_at = now() - interval '1 second' WHERE destination = 'fresh' AND version = 1`); err != nil {
		t.Fatal(err)
	}
	s.expectSent(s.store, "fresh", r3.SigningSecret)
	if s.lastSignedBy("fresh", secrets["fresh"]) {
		t.Fatal("a secret past its overlap still signs")
	}
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.stored("fresh"); st.sealed[0] != nil {
		t.Fatal("a secret past its overlap was not erased")
	}
	listed := s.request("GET", "/v1/operator/destinations", "", "", viewOperator, "")
	if strings.Contains(listed.Body.String(), r3.SigningSecret) {
		t.Fatal("a listing disclosed a signing secret")
	}
	status := s.request("GET", "/v1/operator/destination-secrets", "", "", viewOperator, "")
	s.expect(status, 200)
	if !strings.Contains(status.Body.String(), `"signing_available":true`) || strings.Contains(status.Body.String(), r3.SigningSecret) {
		t.Fatalf("status %s", status.Body.String())
	}
}

// Rollback restores each destination's newest secret as plaintext, so the release before sealing signs
// exactly as the current one does.
func TestSecretMigrationRollback(t *testing.T) {
	s, secrets := setupSecrets(t, "legacy_a")
	ctx := context.Background()
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.FinalizeSecretMigration(ctx); err != nil {
		t.Fatal(err)
	}
	report, err := s.store.RollbackSecretMigration(ctx)
	if err != nil || len(report.Changed) != 2 || len(report.Refused) != 0 {
		t.Fatalf("rollback %+v %v", report, err)
	}
	for _, name := range []string{"legacy_a", "fresh"} {
		if st := s.stored(name); !bytes.Equal(st.plaintext, mustHex(t, secrets[name])) {
			t.Fatalf("%s: plaintext not restored", name)
		}
	}
	// The release before sealing reads only the plaintext.
	if _, err := s.pool.Exec(ctx, `DELETE FROM destination_secrets`); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"legacy_a", "fresh"} {
		s.expectSent(platform.Store{Pool: s.pool}, name, secrets[name])
	}
}

func decodeInto(t *testing.T, body []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(body, v); err != nil {
		t.Fatal(err)
	}
}

// An activation delivery whose secret cannot be opened waits, within its attempts, and is delivered
// signed once the key is configured again; nothing is sent unsigned.
func TestActivationDeliveryWaitsForSigningKey(t *testing.T) {
	hook := newReceiver(t)
	s := setupActivation(t, hook)
	s.goldSegment()
	ada := s.anchor(linkCustomerA)
	secret := s.destination("crm_hook", hook)
	activation := s.activate("gold", "crm_hook")
	keyless := s.dispatcher
	keyless.Store = platform.Store{Pool: s.pool}
	keyless.Backoff = func(int32) time.Duration { return time.Hour }
	if _, err := keyless.DispatchDue(context.Background(), 100); err != nil {
		t.Fatal(err)
	}
	if len(hook.deliveries()) != 0 {
		t.Fatal("sent without an openable secret")
	}
	if d := deliveryByAnchor(s.status(activation.ID), ada); d.State != "pending" || len(d.Receipts) != 1 || d.Receipts[0].Reason != "signing_key_unavailable" {
		t.Fatalf("ada while the key is missing %+v", d)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE activation_deliveries SET next_attempt_at = now()`); err != nil {
		t.Fatal(err)
	}
	s.dispatch()
	if d := deliveryByAnchor(s.status(activation.ID), ada); d.State != "delivered" {
		t.Fatalf("ada after the key returned %+v", d)
	}
	for _, got := range hook.deliveries() {
		if !signedBy(got.signature, got.body, secret) {
			t.Fatal("a delivery does not verify")
		}
	}
}
