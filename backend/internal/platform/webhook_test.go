package platform

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func TestPostWebhookClassifiesEveryAnswer(t *testing.T) {
	secret := []byte(strings.Repeat("s", 32))
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got, body = r, string(raw)
		code, _ := strconv.Atoi(r.URL.Query().Get("status"))
		w.WriteHeader(code)
	}))
	defer srv.Close()
	for status, want := range map[int]webhookOutcome{200: webhookAccepted, 204: webhookAccepted, 408: webhookTransient, 429: webhookTransient,
		500: webhookTransient, 503: webhookTransient, 400: webhookRejected, 404: webhookRejected, 410: webhookRejected} {
		outcome, code, err := postWebhook(context.Background(), srv.Client(), srv.URL+"?status="+strconv.Itoa(status), "key-1",
			http.Header{"Cf-Delivery-Attempt": {"2"}}, [][]byte{secret}, []byte(`{"a":1}`))
		if err != nil || outcome != want || code != int32(status) {
			t.Errorf("%d: %v %d %v", status, outcome, code, err)
		}
	}
	if got.Method != http.MethodPost || got.Header.Get("Content-Type") != "application/json" || got.Header.Get("Idempotency-Key") != "key-1" ||
		got.Header.Get("CF-Delivery-Attempt") != "2" || body != `{"a":1}` ||
		!strings.HasPrefix(got.Header.Get("CF-Signature"), "t=") || strings.Count(got.Header.Get("CF-Signature"), "v1=") != 1 {
		t.Fatalf("request %v %q", got.Header, body)
	}

	// Every live secret signs: the header verifies under each.
	older := []byte(strings.Repeat("o", 32))
	if _, _, err := postWebhook(context.Background(), srv.Client(), srv.URL+"?status=200", "key-4", nil, [][]byte{secret, older}, []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	fields := strings.Split(got.Header.Get("CF-Signature"), ",")
	for i, key := range [][]byte{secret, older} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(strings.TrimPrefix(fields[0], "t=") + ".{}"))
		if len(fields) != 3 || fields[i+1] != "v1="+hex.EncodeToString(mac.Sum(nil)) {
			t.Fatalf("signature %v does not carry secret %d", fields, i)
		}
	}

	// No answer is transient with no status; our own cancellation is an error, not an outcome.
	srv.Close()
	if outcome, code, err := postWebhook(context.Background(), srv.Client(), srv.URL, "key-2", nil, [][]byte{secret}, nil); err != nil || outcome != webhookTransient || code != 0 {
		t.Fatalf("closed server: %v %d %v", outcome, code, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := postWebhook(ctx, srv.Client(), srv.URL, "key-3", nil, [][]byte{secret}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled send reported %v", err)
	}
}
