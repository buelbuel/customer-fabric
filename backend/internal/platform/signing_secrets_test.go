package platform

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

func TestParseSealingKeys(t *testing.T) {
	a, b := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
	for _, bad := range []string{
		"k1:" + a[:62], "k1:" + a + "00", "K1:" + a, "k1", ":" + a, "k1:zz" + a[2:],
		"k1:" + a + ",k1:" + b, "k1:" + a + ",k2:" + a, strings.Repeat("k:"+a+",", 10) + "k:" + a,
	} {
		if _, err := ParseSealingKeys(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	keys, err := ParseSealingKeys(" k1:" + a + ", k0:" + b + "\n")
	if err != nil || len(keys.keys) != 2 || keys.keys[0].id != "k1" {
		t.Fatalf("%+v %v", keys, err)
	}
	if empty, err := ParseSealingKeys(""); err != nil || empty.Configured() {
		t.Fatal("empty configuration")
	}
}

func TestSealedSecretsAreBoundToTheirRow(t *testing.T) {
	keys, _ := ParseSealingKeys("k1:" + strings.Repeat("ab", 32))
	other, _ := ParseSealingKeys("k1:" + strings.Repeat("cd", 32))
	secret := bytes.Repeat([]byte{7}, 32)
	keyID := pgtype.Text{String: "k1", Valid: true}
	sealed, err := keys.keys[0].seal(secret, secretContext("crm", 2))
	if err != nil || len(sealed) != 60 || bytes.Contains(sealed, secret) {
		t.Fatalf("seal %v", err)
	}
	if opened, err := keys.open("crm", 2, keyID, sealed); err != nil || !bytes.Equal(opened, secret) {
		t.Fatal("does not open")
	}
	for name, open := range map[string]func() ([]byte, error){
		"another destination": func() ([]byte, error) { return keys.open("erp", 2, keyID, sealed) },
		"another version":     func() ([]byte, error) { return keys.open("crm", 1, keyID, sealed) },
		"another key":         func() ([]byte, error) { return other.open("crm", 2, keyID, sealed) },
		"an unknown key ID":   func() ([]byte, error) { return keys.open("crm", 2, pgtype.Text{String: "k2", Valid: true}, sealed) },
		"truncated":           func() ([]byte, error) { return keys.open("crm", 2, keyID, sealed[:59]) },
	} {
		if _, err := open(); err == nil {
			t.Errorf("opened with %s", name)
		}
	}
	nonce := append([]byte{}, sealed...)
	nonce[20] ^= 1
	if _, err := keys.open("crm", 2, keyID, nonce); err == nil {
		t.Error("opened a tampered value")
	}
}

func TestSignatureCarriesEveryLiveSecret(t *testing.T) {
	newer, older := bytes.Repeat([]byte{1}, 32), bytes.Repeat([]byte{2}, 32)
	at := time.Unix(1700000000, 0)
	header := signature([][]byte{newer, older}, at, []byte(`{}`))
	fields := strings.Split(header, ",")
	if len(fields) != 3 || fields[0] != "t=1700000000" || !strings.HasPrefix(fields[1], "v1=") || fields[1] == fields[2] {
		t.Fatalf("header %q", header)
	}
	if single := signature([][]byte{newer}, at, []byte(`{}`)); single != fields[0]+","+fields[1] {
		t.Fatalf("a single-secret header changed: %q", single)
	}
}
