package platform

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// signature signs a send with every live secret: "t=<unix seconds>" and one "v1=<hex HMAC-SHA256 of
// "<t>.<body>">" per secret, newest first. A recipient accepts the send if any v1 matches its secret.
func signature(secrets [][]byte, at time.Time, body []byte) string {
	timestamp := strconv.FormatInt(at.Unix(), 10)
	header := "t=" + timestamp
	for _, secret := range secrets {
		header += ",v1=" + hex.EncodeToString(sign(secret, timestamp, body))
	}
	return header
}

func sign(secret []byte, timestamp string, body []byte) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(timestamp + "."))
	mac.Write(body)
	return mac.Sum(nil)
}

// SealingKeys encrypt webhook signing secrets at rest with AES-256-GCM. The first key seals every new or
// resealed secret; each key still opens what it sealed, so a deployment rotates by putting a new key first
// and removing the old one once the housekeeper has resealed everything under the new one.
type SealingKeys struct{ keys []sealingKey }

type sealingKey struct {
	id   string
	aead cipher.AEAD
}

var sealingKeyID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)

// ParseSealingKeys reads "id:hex,id:hex" with 32-byte keys, the sealing key first. Empty is no keys.
func ParseSealingKeys(raw string) (SealingKeys, error) {
	var k SealingKeys
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return k, nil
	}
	parts := strings.Split(raw, ",")
	if len(parts) > 10 {
		return k, errors.New("configure at most 10 webhook secret keys")
	}
	seen := map[string]bool{}
	for _, part := range parts {
		id, material, _ := strings.Cut(strings.TrimSpace(part), ":")
		key, err := hex.DecodeString(material)
		if !sealingKeyID.MatchString(id) || err != nil || len(key) != 32 || seen[id] || seen[string(key)] {
			return SealingKeys{}, errors.New("webhook secret keys must be distinct id:hex pairs of 32-byte keys")
		}
		seen[id], seen[string(key)] = true, true
		block, err := aes.NewCipher(key)
		if err != nil {
			return SealingKeys{}, err
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return SealingKeys{}, err
		}
		k.keys = append(k.keys, sealingKey{id: id, aead: aead})
	}
	return k, nil
}

// Configured reports whether any key is configured.
func (k SealingKeys) Configured() bool { return len(k.keys) > 0 }

// ErrSigningKeyUnavailable means a destination's secret is sealed under a key that is not configured or
// does not open it. Nothing is sent and nothing is changed: the send waits for the key.
var ErrSigningKeyUnavailable = errors.New("the destination's signing secret cannot be opened with the configured keys")

var errSealedUnknownKey = errors.New("sealed under a key that is not configured")

// secretContext binds a sealed secret to its destination and version, so a sealed value copied to another
// row does not open.
func secretContext(destination string, version int32) []byte {
	return []byte("customer-fabric.destination-secret.v1\x00" + destination + "\x00" + strconv.Itoa(int(version)))
}

func (k SealingKeys) seal(destination string, version int32, secret []byte) (pgtype.Text, []byte, error) {
	if len(k.keys) == 0 {
		return pgtype.Text{}, nil, errors.New("no webhook secret key configured")
	}
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return pgtype.Text{}, nil, err
	}
	key := k.keys[0]
	return pgtype.Text{String: key.id, Valid: true}, key.aead.Seal(nonce, nonce, secret, secretContext(destination, version)), nil
}

func (k SealingKeys) open(destination string, version int32, keyID pgtype.Text, sealed []byte) ([]byte, error) {
	for _, key := range k.keys {
		if key.id == keyID.String {
			if len(sealed) != 60 {
				return nil, errors.New("malformed sealed secret")
			}
			return key.aead.Open(nil, sealed[:12], sealed[12:], secretContext(destination, version))
		}
	}
	return nil, errSealedUnknownKey
}

func keyCheckContext(id string) []byte {
	return []byte("customer-fabric.sealing-key-check.v1\x00" + id)
}

// ready checks the sealing key before anything is sealed with it. The first use of a key ID records a
// check value sealed under it; a key configured under that ID later must open it, or it is the wrong key
// and the error is ErrSigningKeyUnavailable.
func (k SealingKeys) ready(ctx context.Context, q *db.Queries) error {
	if len(k.keys) == 0 {
		return ErrSigningKeyUnavailable
	}
	key := k.keys[0]
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return err
	}
	check := key.aead.Seal(nonce, nonce, make([]byte, 32), keyCheckContext(key.id))
	if err := q.RegisterSealingKey(ctx, db.RegisterSealingKeyParams{ID: key.id, CheckValue: check}); err != nil {
		return err
	}
	stored, err := q.SealingKeyCheck(ctx, key.id)
	if err != nil {
		return err
	}
	if !key.matches(stored) {
		return ErrSigningKeyUnavailable
	}
	return nil
}

func (k sealingKey) matches(check []byte) bool {
	if len(check) != 60 {
		return false
	}
	_, err := k.aead.Open(nil, check[:12], check[12:], keyCheckContext(k.id))
	return err == nil
}

// sealVerified seals secret as a destination's version and checks, before anything relies on it, that
// the sealed value opens to a secret that signs exactly as secret does.
func (k SealingKeys) sealVerified(destination string, version int32, secret []byte) (pgtype.Text, []byte, error) {
	keyID, sealed, err := k.seal(destination, version, secret)
	if err != nil {
		return keyID, nil, err
	}
	opened, err := k.open(destination, version, keyID, sealed)
	if err != nil || !sameSignature(opened, secret) {
		return keyID, nil, errors.New("a sealed signing secret did not verify")
	}
	return keyID, sealed, nil
}

// sameSignature reports whether two 32-byte secrets sign a probe identically.
func sameSignature(a, b []byte) bool {
	probe := []byte("customer-fabric.signature-probe")
	return len(a) == 32 && len(b) == 32 && hmac.Equal(sign(a, "0", probe), sign(b, "0", probe))
}

// issueSecret creates a new secret as a locked destination's version and returns it, the only time it is
// returned in the clear.
func (k SealingKeys) issueSecret(ctx context.Context, q *db.Queries, destination string, version int32) ([]byte, error) {
	if err := k.ready(ctx, q); err != nil {
		return nil, err
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	keyID, sealed, err := k.sealVerified(destination, version, secret)
	if err != nil {
		return nil, err
	}
	return secret, q.InsertDestinationSecret(ctx, db.InsertDestinationSecretParams{Destination: destination, Version: version, KeyID: keyID, Sealed: sealed})
}

// signingSecrets returns the secrets that sign a send to a destination now, newest first. A destination
// whose plaintext secret has not been sealed yet signs with it, as it always has. Once it has a sealed
// secret, only live sealed secrets sign; if the newest cannot be opened, nothing is sent and the error is
// ErrSigningKeyUnavailable. An older secret that cannot be opened is left out.
func (s Store) signingSecrets(ctx context.Context, q *db.Queries, d db.Destination) ([][]byte, error) {
	sealed, err := q.DestinationHasSealedSecret(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	if !sealed {
		if len(d.SigningSecret) != 32 {
			return nil, ErrSigningKeyUnavailable
		}
		return [][]byte{d.SigningSecret}, nil
	}
	live, err := q.LiveDestinationSecrets(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	var secrets [][]byte
	for i, row := range live {
		secret, err := s.Keys.open(d.Name, row.Version, row.KeyID, row.Sealed)
		if err != nil && i == 0 {
			return nil, ErrSigningKeyUnavailable
		}
		if err == nil {
			secrets = append(secrets, secret)
		}
	}
	if len(secrets) == 0 {
		return nil, ErrSigningKeyUnavailable
	}
	return secrets, nil
}

// SecretRotation is a destination's new signing secret, returned once. The previous secret, if any, also
// signs every send until PreviousValidUntil.
type SecretRotation struct {
	Destination        string     `json:"destination"`
	Version            int32      `json:"version"`
	SigningSecret      string     `json:"signing_secret"`
	PreviousValidUntil *time.Time `json:"previous_valid_until"`
}

// MaxSecretOverlap bounds how long a rotated secret keeps signing.
const MaxSecretOverlap = 7 * 24 * time.Hour

// RotateDestinationSecret issues a destination a new signing secret. The current secret keeps signing for
// overlap, so the recipient can accept both while it switches; every older secret is erased now. A zero
// overlap erases the current secret at once, for a secret that leaked. A plaintext secret not yet sealed
// is sealed first, so it can keep signing during the overlap, and is then cleared.
func (s Store) RotateDestinationSecret(ctx context.Context, destination string, overlap time.Duration) (SecretRotation, error) {
	r := SecretRotation{Destination: destination}
	if overlap < 0 || overlap > MaxSecretOverlap {
		return r, problem(422, "Invalid rotation", "overlap_seconds must be between 0 and 604800.")
	}
	if !s.Keys.Configured() {
		return r, errKeyProblem
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		d, err := q.LockDestination(ctx, destination)
		if errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "Unknown destination.")
		}
		if err != nil {
			return err
		}
		if err = s.sealPlaintext(ctx, q, d); err != nil {
			return err
		}
		current, err := q.LatestDestinationSecretVersion(ctx, destination)
		if err != nil {
			return err
		}
		live, err := q.LiveDestinationSecrets(ctx, destination)
		if err != nil {
			return err
		}
		// A current secret that does not open means the keys are wrong: nothing is replaced.
		if len(live) > 0 {
			if _, err := s.Keys.open(destination, live[0].Version, live[0].KeyID, live[0].Sealed); err != nil {
				return ErrSigningKeyUnavailable
			}
		}
		newest := int32(0)
		if overlap > 0 && len(live) > 0 && live[0].Version == current {
			newest = current
		}
		until := time.Now().Add(overlap).UTC().Truncate(time.Microsecond)
		if err = q.RetireDestinationSecrets(ctx, db.RetireDestinationSecretsParams{Destination: destination, Newest: newest, RetiresAt: until}); err != nil {
			return err
		}
		if newest > 0 {
			r.PreviousValidUntil = &until
		}
		if d.SigningSecret != nil {
			if err = q.ClearPlaintextSecret(ctx, destination); err != nil {
				return err
			}
		}
		r.Version = current + 1
		secret, err := s.Keys.issueSecret(ctx, q, destination, r.Version)
		r.SigningSecret = hex.EncodeToString(secret)
		return err
	})
	if errors.Is(err, ErrSigningKeyUnavailable) {
		return SecretRotation{Destination: destination}, errKeyProblem
	}
	if err != nil {
		return SecretRotation{Destination: destination}, err
	}
	return r, nil
}

// errKeyProblem answers a request that would seal under keys that are missing or wrong.
var errKeyProblem = problem(503, "Signing key unavailable", "The configured webhook secret keys are missing or do not match the keys that sealed existing secrets.")

// sealPlaintext seals a locked destination's plaintext secret as version 1 if it has no sealed secret
// yet, verified to sign as the plaintext does. The plaintext stays.
func (s Store) sealPlaintext(ctx context.Context, q *db.Queries, d db.Destination) error {
	if d.SigningSecret == nil {
		return nil
	}
	sealed, err := q.DestinationHasSealedSecret(ctx, d.Name)
	if err != nil || sealed {
		return err
	}
	if err = s.Keys.ready(ctx, q); err != nil {
		return err
	}
	keyID, value, err := s.Keys.sealVerified(d.Name, 1, d.SigningSecret)
	if err != nil {
		return err
	}
	return q.InsertDestinationSecret(ctx, db.InsertDestinationSecretParams{Destination: d.Name, Version: 1, KeyID: keyID, Sealed: value})
}

// SealPlaintextSecrets seals up to limit plaintext secrets that have none sealed yet, in one transaction,
// and keeps the plaintext. It is safe to interrupt and repeat. It returns how many it sealed.
func (s Store) SealPlaintextSecrets(ctx context.Context, limit int32) (int, error) {
	if !s.Keys.Configured() {
		return 0, nil
	}
	sealed := 0
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		if err := s.Keys.ready(ctx, q); err != nil {
			return err
		}
		rows, err := q.ClaimPlaintextDestinations(ctx, limit)
		if err != nil {
			return err
		}
		for _, row := range rows {
			keyID, value, err := s.Keys.sealVerified(row.Name, 1, row.SigningSecret)
			if err != nil {
				return err
			}
			if err = q.InsertDestinationSecret(ctx, db.InsertDestinationSecretParams{Destination: row.Name, Version: 1, KeyID: keyID, Sealed: value}); err != nil {
				return err
			}
			sealed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return sealed, nil
}

// ResealSecrets moves secrets sealed under an older configured key to the sealing key, limit per
// transaction. A secret that does not open is left exactly as it is. It returns how many it moved.
func (s Store) ResealSecrets(ctx context.Context, limit int32) (int, error) {
	if !s.Keys.Configured() {
		return 0, nil
	}
	moved, skip := 0, []string{}
	for ctx.Err() == nil {
		batch, unopened := 0, 0
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			q := db.New(s.Pool).WithTx(tx)
			if err := s.Keys.ready(ctx, q); err != nil {
				return err
			}
			rows, err := q.ClaimSecretsToReseal(ctx, db.ClaimSecretsToResealParams{KeyID: pgtype.Text{String: s.Keys.keys[0].id, Valid: true}, Skip: skip, RowLimit: limit})
			if err != nil {
				return err
			}
			for _, row := range rows {
				secret, err := s.Keys.open(row.Destination, row.Version, row.KeyID, row.Sealed)
				if err != nil {
					skip = append(skip, row.Destination)
					unopened++
					continue
				}
				keyID, value, err := s.Keys.sealVerified(row.Destination, row.Version, secret)
				if err != nil {
					return err
				}
				if err = q.ResealDestinationSecret(ctx, db.ResealDestinationSecretParams{Destination: row.Destination, Version: row.Version, KeyID: keyID, Sealed: value}); err != nil {
					return err
				}
				batch++
			}
			return nil
		})
		if err != nil {
			return moved, err
		}
		moved += batch
		if batch == 0 && unopened == 0 {
			break
		}
	}
	return moved, ctx.Err()
}

// EraseRetiredSecrets erases every secret past its overlap.
func (s Store) EraseRetiredSecrets(ctx context.Context) (int64, error) {
	return db.New(s.Pool).EraseRetiredDestinationSecrets(ctx)
}

// SecretStatus is what an operator needs to finish a migration or a key rotation: per state, the
// destinations in it. It never contains a secret.
type SecretStatus struct {
	// SigningAvailable is false while any destination's secret cannot be opened.
	SigningAvailable bool `json:"signing_available"`
	// MigrationComplete is true once no destination keeps a plaintext secret.
	MigrationComplete bool `json:"migration_complete"`
	// RotationComplete is true once every live secret is sealed under the sealing key, so older keys
	// can be removed.
	RotationComplete bool `json:"rotation_complete"`
	// SealingKey is the key that seals new secrets; empty when none is configured.
	SealingKey string `json:"sealing_key"`
	// WrongKeys are configured key IDs whose key does not match the key first used under that ID.
	WrongKeys []string `json:"wrong_keys"`
	// Plaintext destinations still sign with a secret that is not sealed yet.
	Plaintext []string `json:"plaintext"`
	// Sealed destinations sign with sealed secrets that open.
	Sealed []string `json:"sealed"`
	// Unfinalized destinations have a sealed secret and still keep their plaintext.
	Unfinalized []string `json:"unfinalized"`
	// Unavailable destinations have a newest secret the configured keys cannot open: nothing is sent to
	// them until the key that sealed it is configured.
	Unavailable []string `json:"unavailable"`
	// Stale destinations have a live secret sealed under a key other than the sealing key.
	Stale []string `json:"stale"`
}

// SecretStatus opens every live secret to report which destinations can sign now.
func (s Store) SecretStatus(ctx context.Context) (SecretStatus, error) {
	st := SecretStatus{WrongKeys: []string{}, Plaintext: []string{}, Sealed: []string{}, Unfinalized: []string{}, Unavailable: []string{}, Stale: []string{}}
	if s.Keys.Configured() {
		st.SealingKey = s.Keys.keys[0].id
	}
	q := db.New(s.Pool)
	for _, key := range s.Keys.keys {
		check, err := q.SealingKeyCheck(ctx, key.id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return st, err
		}
		if err == nil && !key.matches(check) {
			st.WrongKeys = append(st.WrongKeys, key.id)
		}
	}
	states, err := q.DestinationSecretStates(ctx)
	if err != nil {
		return st, err
	}
	for _, d := range states {
		if !d.HasSealed {
			st.Plaintext = append(st.Plaintext, d.Name)
			continue
		}
		if d.HasPlaintext {
			st.Unfinalized = append(st.Unfinalized, d.Name)
		}
		live, err := q.LiveDestinationSecrets(ctx, d.Name)
		if err != nil {
			return st, err
		}
		available, stale := len(live) > 0, false
		for i, row := range live {
			if _, err := s.Keys.open(d.Name, row.Version, row.KeyID, row.Sealed); err != nil {
				available = available && i > 0
			} else if row.KeyID.String != st.SealingKey {
				stale = true
			}
		}
		if available {
			st.Sealed = append(st.Sealed, d.Name)
		} else {
			st.Unavailable = append(st.Unavailable, d.Name)
		}
		if stale {
			st.Stale = append(st.Stale, d.Name)
		}
	}
	st.SigningAvailable = len(st.Unavailable) == 0 && len(st.WrongKeys) == 0
	st.MigrationComplete = len(st.Plaintext) == 0 && len(st.Unfinalized) == 0
	st.RotationComplete = len(st.Stale) == 0 && len(st.Unavailable) == 0
	return st, nil
}

// MigrationReport is what a finalize or rollback changed, and what it refused to change.
type MigrationReport struct {
	Changed []string `json:"changed"`
	Refused []string `json:"refused"`
}

// FinalizeSecretMigration clears the plaintext of every destination whose sealed secret is verified
// again, under the destination's lock, to open and to sign exactly as its plaintext does. A destination
// whose plaintext has no such sealed secret keeps its plaintext and is reported as refused.
func (s Store) FinalizeSecretMigration(ctx context.Context) (MigrationReport, error) {
	return s.eachDestination(ctx, func(ctx context.Context, q *db.Queries, d db.Destination) (bool, error) {
		if d.SigningSecret == nil {
			return false, nil
		}
		live, err := q.LiveDestinationSecrets(ctx, d.Name)
		if err != nil {
			return false, err
		}
		for _, row := range live {
			if secret, err := s.Keys.open(d.Name, row.Version, row.KeyID, row.Sealed); err == nil && sameSignature(secret, d.SigningSecret) {
				return true, q.ClearPlaintextSecret(ctx, d.Name)
			}
		}
		return false, errRefused
	})
}

// RollbackSecretMigration restores, for every destination without a plaintext secret, its newest sealed
// secret as plaintext, so the release before sealing can sign again. It is the only path that writes a
// secret in the clear, and only on an operator's explicit downgrade. A destination whose newest secret
// does not open is reported as refused and left unchanged.
func (s Store) RollbackSecretMigration(ctx context.Context) (MigrationReport, error) {
	return s.eachDestination(ctx, func(ctx context.Context, q *db.Queries, d db.Destination) (bool, error) {
		if d.SigningSecret != nil {
			return false, nil
		}
		live, err := q.LiveDestinationSecrets(ctx, d.Name)
		if err != nil {
			return false, err
		}
		if len(live) == 0 {
			return false, errRefused
		}
		secret, err := s.Keys.open(d.Name, live[0].Version, live[0].KeyID, live[0].Sealed)
		if err != nil {
			return false, errRefused
		}
		return true, q.RestorePlaintextSecret(ctx, db.RestorePlaintextSecretParams{Name: d.Name, SigningSecret: secret})
	})
}

var errRefused = errors.New("refused")

// eachDestination runs change on every destination, each in its own transaction under the destination's
// lock, so an interrupted run leaves every destination either changed or not and can be repeated.
func (s Store) eachDestination(ctx context.Context, change func(context.Context, *db.Queries, db.Destination) (bool, error)) (MigrationReport, error) {
	report := MigrationReport{Changed: []string{}, Refused: []string{}}
	states, err := db.New(s.Pool).DestinationSecretStates(ctx)
	if err != nil {
		return report, err
	}
	for _, state := range states {
		changed := false
		err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
			q := db.New(s.Pool).WithTx(tx)
			d, err := q.LockDestination(ctx, state.Name)
			if err != nil {
				return err
			}
			changed, err = change(ctx, q, d)
			return err
		})
		switch {
		case errors.Is(err, errRefused):
			report.Refused = append(report.Refused, state.Name)
		case err != nil:
			return report, err
		case changed:
			report.Changed = append(report.Changed, state.Name)
		}
	}
	return report, nil
}
