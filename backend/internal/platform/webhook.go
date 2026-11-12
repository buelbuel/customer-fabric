package platform

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"
)

// webhookOutcome is how one signed send ended, the same for activation deliveries and change
// notifications. What each does next (retry, settle, record) is its own.
type webhookOutcome int

const (
	// webhookAccepted is a 2xx answer.
	webhookAccepted webhookOutcome = iota
	// webhookTransient is a 408, 429 or 5xx answer, or a transport error: a later attempt may succeed.
	webhookTransient
	// webhookRejected is any other answer: retrying cannot help.
	webhookRejected
	// webhookRefused means the egress policy refused the connected address: nothing was sent.
	webhookRefused
)

// postWebhook sends body as one JSON POST to target under the egress policy of client, signed with every
// live secret and keyed by idempotencyKey, with any extra headers. It returns the outcome and the status
// code, which is 0 when no answer arrived. A send cut short by ctx returns ctx's error: our own shutdown
// says nothing about the destination.
func postWebhook(ctx context.Context, client *http.Client, target, idempotencyKey string, extra http.Header, secrets [][]byte, body []byte) (webhookOutcome, int32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	for name, values := range extra {
		req.Header[name] = values
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", idempotencyKey)
	req.Header.Set("CF-Signature", signature(secrets, time.Now(), body))
	res, err := client.Do(req)
	switch {
	case errors.Is(err, errEgressRefused):
		return webhookRefused, 0, nil
	case err != nil && ctx.Err() != nil:
		return 0, 0, ctx.Err()
	case err != nil:
		return webhookTransient, 0, nil
	}
	res.Body.Close()
	code := int32(res.StatusCode)
	switch {
	case code >= 200 && code < 300:
		return webhookAccepted, code, nil
	case code == http.StatusRequestTimeout || code == http.StatusTooManyRequests || code >= 500:
		return webhookTransient, code, nil
	}
	return webhookRejected, code, nil
}
