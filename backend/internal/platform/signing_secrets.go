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
		mac := hmac.New(sha256.New, secret)
		mac.Write([]byte(timestamp + "."))
		mac.Write(body)
		header += ",v1=" + hex.EncodeToString(mac.Sum(nil))
	}
	return header
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

// ErrSigningKeyUnavailable means the configured keys are missing or wrong: a destination's secret cannot
// be opened, or a new one cannot be sealed. Nothing is sent and nothing stored changes; the work waits for
// the key.
var ErrSigningKeyUnavailable = errors.New("webhook signing secrets cannot be opened or sealed with the configured keys")

// errKeyProblem answers a request that needs keys that are missing or wrong.
var errKeyProblem = problem(503, "Signing key unavailable", "The configured webhook secret keys are missing or do not match the keys that sealed existing secrets.")

// secretContext binds a sealed secret to its destination and version, so a sealed value copied to another
// row does not open.
func secretContext(destination string, version int32) []byte {
	return []byte("customer-fabric.destination-secret.v1\x00" + destination + "\x00" + strconv.Itoa(int(version)))
}

func keyCheckContext(id string) []byte {
	return []byte("customer-fabric.sealing-key-check.v1\x00" + id)
}

func (k sealingKey) seal(secret, context []byte) ([]byte, error) {
	nonce := make([]byte, 12)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return k.aead.Seal(nonce, nonce, secret, context), nil
}

func (k sealingKey) open(sealed, context []byte) ([]byte, error) {
	if len(sealed) != 60 {
		return nil, errors.New("malformed sealed value")
	}
	return k.aead.Open(nil, sealed[:12], sealed[12:], context)
}

func (k SealingKeys) open(destination string, version int32, keyID pgtype.Text, sealed []byte) ([]byte, error) {
	for _, key := range k.keys {
		if key.id == keyID.String {
			return key.open(sealed, secretContext(destination, version))
		}
	}
	return nil, ErrSigningKeyUnavailable
}

// sealing returns the sealing key after checking it. The first use of a key ID records a check value
// sealed under it; a key configured under that ID later must open it, or it is the wrong key and nothing
// is sealed with it.
func (k SealingKeys) sealing(ctx context.Context, q *db.Queries) (sealingKey, error) {
	if len(k.keys) == 0 {
		return sealingKey{}, ErrSigningKeyUnavailable
	}
	key := k.keys[0]
	check, err := key.seal(make([]byte, 32), keyCheckContext(key.id))
	if err != nil {
		return key, err
	}
	if err = q.RegisterSealingKey(ctx, db.RegisterSealingKeyParams{ID: key.id, CheckValue: check}); err != nil {
		return key, err
	}
	if check, err = q.SealingKeyCheck(ctx, key.id); err != nil {
		return key, err
	}
	if _, err = key.open(check, keyCheckContext(key.id)); err != nil {
		return key, ErrSigningKeyUnavailable
	}
	return key, nil
}

// sealSecret seals secret as a destination's version under the checked sealing key.
func (k SealingKeys) sealSecret(ctx context.Context, q *db.Queries, destination string, version int32, secret []byte) (pgtype.Text, []byte, error) {
	key, err := k.sealing(ctx, q)
	if err != nil {
		return pgtype.Text{}, nil, err
	}
	sealed, err := key.seal(secret, secretContext(destination, version))
	return pgtype.Text{String: key.id, Valid: true}, sealed, err
}

// issueSecret creates a new secret as a locked destination's version and returns it, the only time it is
// returned in the clear.
func (k SealingKeys) issueSecret(ctx context.Context, q *db.Queries, destination string, version int32) ([]byte, error) {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return nil, err
	}
	keyID, sealed, err := k.sealSecret(ctx, q, destination, version, secret)
	if err != nil {
		return nil, err
	}
	return secret, q.InsertDestinationSecret(ctx, db.InsertDestinationSecretParams{Destination: destination, Version: version, KeyID: keyID, Sealed: sealed})
}

// signingSecrets returns the live secrets that sign a send to a destination now, newest first. If the
// newest cannot be opened, nothing is sent and the error is ErrSigningKeyUnavailable. An older secret that
// cannot be opened is left out.
func (s Store) signingSecrets(ctx context.Context, q *db.Queries, destination string) ([][]byte, error) {
	live, err := q.LiveDestinationSecrets(ctx, destination)
	if err != nil {
		return nil, err
	}
	var secrets [][]byte
	for i, row := range live {
		secret, err := s.Keys.open(destination, row.Version, row.KeyID, row.Sealed)
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

// SecretRotation is a destination's new signing secret, returned once. The previous secret also signs
// every send until PreviousValidUntil, if set.
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
// overlap erases the current secret at once, for a secret that leaked. With keys that cannot open the
// current secret, nothing is replaced.
func (s Store) RotateDestinationSecret(ctx context.Context, destination string, overlap time.Duration) (SecretRotation, error) {
	r := SecretRotation{Destination: destination}
	if overlap < 0 || overlap > MaxSecretOverlap {
		return r, problem(422, "Invalid rotation", "overlap_seconds must be between 0 and 604800.")
	}
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		q := db.New(s.Pool).WithTx(tx)
		if _, err := q.LockDestination(ctx, destination); errors.Is(err, pgx.ErrNoRows) {
			return problem(404, "Not found", "Unknown destination.")
		} else if err != nil {
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
			key, err := s.Keys.sealing(ctx, q)
			if err != nil {
				return err
			}
			rows, err := q.ClaimSecretsToReseal(ctx, db.ClaimSecretsToResealParams{KeyID: pgtype.Text{String: key.id, Valid: true}, Skip: skip, RowLimit: limit})
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
				sealed, err := key.seal(secret, secretContext(row.Destination, row.Version))
				if err != nil {
					return err
				}
				if err = q.ResealDestinationSecret(ctx, db.ResealDestinationSecretParams{Destination: row.Destination, Version: row.Version,
					KeyID: pgtype.Text{String: key.id, Valid: true}, Sealed: sealed}); err != nil {
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

// SecretStatus says which destinations can sign now and what awaits an operator. It never contains a
// secret.
type SecretStatus struct {
	// SigningAvailable is false while a configured key is wrong or a destination's secret cannot be
	// opened.
	SigningAvailable bool `json:"signing_available"`
	// RotationComplete is true once every live secret is sealed under the sealing key, so older keys can
	// be removed.
	RotationComplete bool `json:"rotation_complete"`
	// SealingKey is the key that seals new secrets; empty when none is configured.
	SealingKey string `json:"sealing_key"`
	// WrongKeys are configured key IDs whose key does not match the key first used under that ID.
	WrongKeys []string `json:"wrong_keys"`
	// Unavailable destinations have a newest secret the configured keys cannot open: nothing is sent to
	// them until the key that sealed it is configured.
	Unavailable []string `json:"unavailable"`
	// Stale destinations have a live secret sealed under a key other than the sealing key.
	Stale []string `json:"stale"`
}

// SecretStatus opens every live secret to report which destinations can sign now.
func (s Store) SecretStatus(ctx context.Context) (SecretStatus, error) {
	st := SecretStatus{WrongKeys: []string{}, Unavailable: []string{}, Stale: []string{}}
	if s.Keys.Configured() {
		st.SealingKey = s.Keys.keys[0].id
	}
	q := db.New(s.Pool)
	for _, key := range s.Keys.keys {
		check, err := q.SealingKeyCheck(ctx, key.id)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return st, err
		}
		if err == nil {
			if _, err := key.open(check, keyCheckContext(key.id)); err != nil {
				st.WrongKeys = append(st.WrongKeys, key.id)
			}
		}
	}
	destinations, err := q.ListDestinations(ctx)
	if err != nil {
		return st, err
	}
	for _, d := range destinations {
		live, err := q.LiveDestinationSecrets(ctx, d.Name)
		if err != nil {
			return st, err
		}
		unavailable, stale := len(live) == 0, false
		for i, row := range live {
			if _, err := s.Keys.open(d.Name, row.Version, row.KeyID, row.Sealed); err != nil {
				unavailable = unavailable || i == 0
			} else if row.KeyID.String != st.SealingKey {
				stale = true
			}
		}
		if unavailable {
			st.Unavailable = append(st.Unavailable, d.Name)
		}
		if stale {
			st.Stale = append(st.Stale, d.Name)
		}
	}
	st.SigningAvailable = len(st.Unavailable) == 0 && len(st.WrongKeys) == 0
	st.RotationComplete = len(st.Stale) == 0 && len(st.Unavailable) == 0
	return st, nil
}
