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

// newKeyPair returns a fresh "id:hex" sealing key.
func newKeyPair(t *testing.T, id string) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return id + ":" + hex.EncodeToString(key)
}

func parseKeys(t *testing.T, pairs ...string) platform.SealingKeys {
	t.Helper()
	keys, err := platform.ParseSealingKeys(strings.Join(pairs, ","))
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

// secretSuite has destinations subscribed to console changes, each on its own receiver, as a recipient
// configured with the destination's secret would be.
type secretSuite struct {
	activationSuite
	record  string
	hooks   map[string]*receiver
	subs    map[string]string
	secrets map[string]string
}

func setupSecrets(t *testing.T, names ...string) secretSuite {
	t.Helper()
	hooks := map[string]*receiver{}
	var all []*receiver
	for _, name := range names {
		hooks[name] = newReceiver(t)
		all = append(all, hooks[name])
	}
	s := secretSuite{activationSuite: setupActivation(t, all...), hooks: hooks, subs: map[string]string{}, secrets: map[string]string{}}
	purpose, _ := s.purposeWithBasis("crm_sync", "legitimate_interests")
	for _, name := range names {
		s.secrets[name] = s.destination(name, hooks[name])
		s.subs[name] = s.subscribe("sync_"+name, subscriptionBody(name, purpose, "console"))
	}
	s.record = s.viewRecord(tokenA, "secret-record", `{"tier":"gold"}`)
	return s
}

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
	sealed [][]byte
	keys   []string
}

func (s secretSuite) stored(destination string) storedSecret {
	s.t.Helper()
	var st storedSecret
	rows, err := s.pool.Query(context.Background(), `SELECT sealed, coalesce(key_id, '') FROM destination_secrets WHERE destination = $1 ORDER BY version`, destination)
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

func (s *secretSuite) useStore(store platform.Store) {
	s.t.Helper()
	api, err := platform.NewAPI(store, s.config)
	if err != nil {
		s.t.Fatal(err)
	}
	s.h = api
}

// The secret is stored only sealed, under the configured key, and every send verifies under the secret
// the recipient was given.
func TestSigningSecretsAreSealedAtRest(t *testing.T) {
	s := setupSecrets(t, "crm")
	st := s.stored("crm")
	secret, _ := hex.DecodeString(s.secrets["crm"])
	if len(st.sealed) != 1 || len(st.sealed[0]) != 60 || st.keys[0] != "test-current" || bytes.Contains(st.sealed[0], secret) {
		t.Fatalf("stored %+v", st)
	}
	var plaintext int
	if err := s.pool.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns WHERE table_name = 'destinations' AND column_name = 'signing_secret'`).Scan(&plaintext); err != nil || plaintext != 0 {
		t.Fatal("destinations still have a plaintext secret column")
	}
	s.expectSent(s.store, "crm", s.secrets["crm"])
	status := s.request("GET", "/v1/operator/destination-secrets", "", "", viewOperator, "")
	s.expect(status, 200)
	if !strings.Contains(status.Body.String(), `"signing_available":true`) || strings.Contains(status.Body.String(), s.secrets["crm"]) {
		t.Fatalf("status %s", status.Body.String())
	}
	s.expect(s.request("GET", "/v1/operator/destination-secrets", "", "", tokenA, ""), 401)
}

// A missing or wrong key holds every send, never unsigned, and changes, regenerates or removes nothing;
// registration and rotation are refused. Configuring the right key resumes.
func TestMissingOrWrongKeyFailsClosed(t *testing.T) {
	s := setupSecrets(t, "crm", "erp")
	before := map[string]storedSecret{"crm": s.stored("crm"), "erp": s.stored("erp")}
	for name, store := range map[string]platform.Store{
		"missing": {Pool: s.pool},
		"wrong":   {Pool: s.pool, Keys: parseKeys(t, newKeyPair(t, "test-current"))},
		"unknown": {Pool: s.pool, Keys: parseKeys(t, newKeyPair(t, "test-other"))},
	} {
		for _, destination := range []string{"crm", "erp"} {
			s.expectHeld(store, destination)
		}
		st := s.status(store)
		if st.SigningAvailable || len(st.Unavailable) != 2 || (name == "wrong") != (len(st.WrongKeys) == 1) {
			t.Fatalf("%s: status %+v", name, st)
		}
		if err := (platform.Housekeeper{Store: store}).Step(context.Background()); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		s.useStore(store)
		s.expect(s.request("POST", "/v1/operator/destinations/crm/signing-secret", `{"overlap_seconds":60}`, "", viewOperator, ""), 503)
		if name != "unknown" {
			s.expect(s.request("POST", "/v1/operator/destinations/another", fmt.Sprintf(`{"url":%q}`, s.hooks["crm"].URL+"/hook"), "", viewOperator, ""), 503)
		}
		for destination, was := range before {
			now := s.stored(destination)
			if len(now.sealed) != len(was.sealed) || !bytes.Equal(now.sealed[0], was.sealed[0]) || now.keys[0] != was.keys[0] {
				t.Fatalf("%s: %s changed under an unusable key", name, destination)
			}
		}
	}
	s.useStore(s.store)
	for _, destination := range []string{"crm", "erp"} {
		s.expectSent(s.store, destination, s.secrets[destination])
	}
}

// Rotating the deployment key reseals every secret under the new key in the background, without any
// recipient noticing; the old key can then be removed.
func TestSealingKeyRotation(t *testing.T) {
	s := setupSecrets(t, "crm", "erp")
	ctx := context.Background()
	next := newKeyPair(t, "test-next")
	rotating := platform.Store{Pool: s.pool, Keys: parseKeys(t, next, testKeyPair)}
	if st := s.status(rotating); st.RotationComplete || len(st.Stale) != 2 || !st.SigningAvailable {
		t.Fatalf("status before resealing %+v", st)
	}
	s.expectSent(rotating, "crm", s.secrets["crm"])
	if err := (platform.Housekeeper{Store: rotating}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.status(rotating); !st.RotationComplete {
		t.Fatalf("status after resealing %+v", st)
	}
	onlyNext := platform.Store{Pool: s.pool, Keys: parseKeys(t, next)}
	for _, name := range []string{"crm", "erp"} {
		if st := s.stored(name); st.keys[0] != "test-next" {
			t.Fatalf("%s still sealed under %s", name, st.keys[0])
		}
		s.expectSent(onlyNext, name, s.secrets[name])
	}
	s.expectHeld(s.store, "crm")

	// A key ID listed after the sealing key but with the wrong key bytes reseals nothing.
	before := map[string]storedSecret{"crm": s.stored("crm"), "erp": s.stored("erp")}
	wrongOld := platform.Store{Pool: s.pool, Keys: parseKeys(t, newKeyPair(t, "test-third"), newKeyPair(t, "test-next"))}
	if err := (platform.Housekeeper{Store: wrongOld}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	for name, was := range before {
		if now := s.stored(name); !bytes.Equal(now.sealed[0], was.sealed[0]) || now.keys[0] != "test-next" {
			t.Fatalf("%s was resealed from a key that does not open it", name)
		}
	}
	if st := s.status(wrongOld); len(st.WrongKeys) != 1 || st.WrongKeys[0] != "test-next" || st.SigningAvailable {
		t.Fatalf("status with a wrong older key %+v", st)
	}
}

// Rotating a destination's secret keeps the previous one signing for the overlap, so a recipient can
// switch without rejecting sends; a zero overlap ends it at once. Erased secrets are gone.
func TestDestinationSecretRotation(t *testing.T) {
	s := setupSecrets(t, "crm")
	ctx := context.Background()
	rotate := func(name, body string, code int) platform.SecretRotation {
		t.Helper()
		res := s.request("POST", "/v1/operator/destinations/"+name+"/signing-secret", body, "", viewOperator, "")
		s.expect(res, code)
		var r platform.SecretRotation
		if code == 201 {
			if err := json.Unmarshal(res.Body.Bytes(), &r); err != nil {
				t.Fatal(err)
			}
		}
		return r
	}
	rotate("missing", `{"overlap_seconds":60}`, 404)
	rotate("crm", `{"overlap_seconds":604801}`, 422)
	rotate("crm", `{}`, 422)
	s.expect(s.request("POST", "/v1/operator/destinations/crm/signing-secret", `{"overlap_seconds":60}`, "", tokenA, ""), 401)

	r := rotate("crm", `{"overlap_seconds":3600}`, 201)
	if r.Version != 2 || r.PreviousValidUntil == nil || len(r.SigningSecret) != 64 {
		t.Fatalf("rotation %+v", r)
	}
	s.expectSent(s.store, "crm", s.secrets["crm"], r.SigningSecret)

	r2 := rotate("crm", `{"overlap_seconds":0}`, 201)
	if r2.Version != 3 || r2.PreviousValidUntil != nil {
		t.Fatalf("rotation %+v", r2)
	}
	s.expectSent(s.store, "crm", r2.SigningSecret)
	if s.lastSignedBy("crm", r.SigningSecret) || s.lastSignedBy("crm", s.secrets["crm"]) {
		t.Fatal("a secret rotated out without overlap still signs")
	}
	if st := s.stored("crm"); st.sealed[0] != nil || st.sealed[1] != nil || st.sealed[2] == nil {
		t.Fatalf("rotated-out secrets were not erased: %+v", st)
	}

	// A previous secret that no longer opens is left out of the signature; the newest still signs.
	r4 := rotate("crm", `{"overlap_seconds":3600}`, 201)
	if _, err := s.pool.Exec(ctx, `UPDATE destination_secrets SET sealed = set_byte(sealed, 20, get_byte(sealed, 20) # 1) WHERE destination = 'crm' AND version = 3`); err != nil {
		t.Fatal(err)
	}
	s.expectSent(s.store, "crm", r4.SigningSecret)
	if s.lastSignedBy("crm", r2.SigningSecret) || strings.Count(s.hooks["crm"].deliveries()[len(s.hooks["crm"].deliveries())-1].signature, "v1=") != 1 {
		t.Fatal("a previous secret that does not open still signed")
	}
	r2 = r4

	// An overlap that ends stops signing, and the housekeeper erases it.
	r3 := rotate("crm", `{"overlap_seconds":60}`, 201)
	s.expectSent(s.store, "crm", r2.SigningSecret, r3.SigningSecret)
	if _, err := s.pool.Exec(ctx, `UPDATE destination_secrets SET retires_at = now() - interval '1 second' WHERE destination = 'crm' AND version = 4`); err != nil {
		t.Fatal(err)
	}
	s.expectSent(s.store, "crm", r3.SigningSecret)
	if s.lastSignedBy("crm", r2.SigningSecret) {
		t.Fatal("a secret past its overlap still signs")
	}
	if err := (platform.Housekeeper{Store: s.store}).Step(ctx); err != nil {
		t.Fatal(err)
	}
	if st := s.stored("crm"); st.sealed[3] != nil {
		t.Fatal("a secret past its overlap was not erased")
	}
	if listed := s.request("GET", "/v1/operator/destinations", "", "", viewOperator, ""); strings.Contains(listed.Body.String(), r3.SigningSecret) {
		t.Fatal("a listing disclosed a signing secret")
	}
}

// Sends racing a rotation with overlap verify under the secret the recipient had, or the new one; none is
// unsigned or signed otherwise.
func TestSendsDuringRotationVerify(t *testing.T) {
	s := setupSecrets(t, "crm")
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if got, err := s.notify(s.store, "crm"); err != nil || got.Outcome != platform.NotificationDelivered {
					errs <- fmt.Errorf("%+v %v", got, err)
					return
				}
			}
		})
	}
	time.Sleep(50 * time.Millisecond)
	rotated, err := s.store.RotateDestinationSecret(context.Background(), "crm", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
	close(stop)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	got := s.hooks["crm"].deliveries()
	if len(got) < 2 || !signedBy(got[len(got)-1].signature, got[len(got)-1].body, rotated.SigningSecret) {
		t.Fatalf("%d sends; the last does not carry the new secret", len(got))
	}
	for _, d := range got {
		if !signedBy(d.signature, d.body, s.secrets["crm"]) {
			t.Fatalf("a send during the overlap does not verify under the previous secret: %q", d.signature)
		}
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
