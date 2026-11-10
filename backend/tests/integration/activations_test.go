package integration

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

// receiver is a synthetic webhook endpoint that answers with scripted status codes.
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	got      []receivedDelivery
}

type receivedDelivery struct {
	body                    string
	key, attempt, signature string
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	r := &receiver{statuses: statuses}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		r.mu.Lock()
		defer r.mu.Unlock()
		r.got = append(r.got, receivedDelivery{string(body), req.Header.Get("Idempotency-Key"), req.Header.Get("CF-Delivery-Attempt"), req.Header.Get("CF-Signature")})
		status := 200
		if len(r.statuses) > 0 {
			status, r.statuses = r.statuses[0], r.statuses[1:]
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(r.Close)
	return r
}

func (r *receiver) deliveries() []receivedDelivery {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]receivedDelivery(nil), r.got...)
}

func (r *receiver) host() string { return strings.TrimPrefix(r.URL, "http://") }

type activationSuite struct {
	linkingSuite
	dispatcher platform.Dispatcher
}

// setupActivation allows deliveries to the given receivers and dispatches without backoff, so a test
// controls every attempt.
func setupActivation(t *testing.T, receivers ...*receiver) activationSuite {
	s := setupLinking(t)
	for _, r := range receivers {
		s.config.WebhookHosts = append(s.config.WebhookHosts, r.host())
	}
	handler, err := platform.NewAPI(platform.Store{Pool: s.pool}, s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.h = handler
	d := platform.NewDispatcher(platform.Store{Pool: s.pool}, s.config.WebhookHosts)
	d.Backoff = func(int32) time.Duration { return 0 }
	return activationSuite{s, d}
}

func (s activationSuite) dispatch() int {
	s.t.Helper()
	n, err := s.dispatcher.DispatchDue(context.Background(), 100)
	if err != nil {
		s.t.Fatal(err)
	}
	return n
}

func (s activationSuite) destination(name string, r *receiver) string {
	s.t.Helper()
	res := s.request("POST", "/v1/operator/destinations/"+name, fmt.Sprintf(`{"url":%q}`, r.URL+"/hook"), "", viewOperator, "")
	s.expect(res, 201)
	var d platform.Destination
	if err := json.Unmarshal(res.Body.Bytes(), &d); err != nil || len(d.SigningSecret) != 64 {
		s.t.Fatalf("destination %s", res.Body.String())
	}
	return d.SigningSecret
}

func (s activationSuite) activate(segment, destination string) platform.ActivationSummary {
	s.t.Helper()
	res := s.request("POST", segmentsPath+"/"+segment+"/activations", fmt.Sprintf(`{"destination":%q}`, destination), "", viewOperator, "")
	s.expect(res, 201)
	var a platform.ActivationSummary
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		s.t.Fatal(err)
	}
	return a
}

func (s activationSuite) status(id string) platform.ActivationStatus {
	s.t.Helper()
	res := s.request("GET", "/v1/operator/activations/"+id, "", "", viewOperator, "")
	s.expect(res, 200)
	var a platform.ActivationStatus
	if err := json.Unmarshal(res.Body.Bytes(), &a); err != nil {
		s.t.Fatal(err)
	}
	return a
}

// goldSegment links two gold customers who both consent and returns the purpose and policy.
func (s activationSuite) goldSegment() (purpose, policy string) {
	s.t.Helper()
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation","activation"],"sources":["console"]}`)
	s.govern("score", `{"type":"integer"}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.viewRecord(tokenA, "act-ada", `{"tier":"gold","score":731}`)
	s.viewRecord(tokenA, "act-bob", `{"tier":"gold","score":732}`)
	s.viewLink(linkIssuerA, linkCustomerA, "act-ada")
	s.viewLink(linkIssuerA, linkCustomerB, "act-bob")
	purpose, policy = s.consentPurpose("activation_email")
	s.decideAsCustomer(linkCustomerA, purpose, policy, "grant")
	s.decideAsCustomer(linkCustomerB, purpose, policy, "grant")
	rule := fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"eq","value":"gold"},{"attribute":"score","operator":"gte","value":700}]}`, purpose)
	s.expect(s.request("POST", segmentsPath+"/gold", rule, "", viewOperator, ""), 201)
	return purpose, policy
}

func deliveryByAnchor(a platform.ActivationStatus, anchor string) platform.DeliveryStatus {
	for _, d := range a.Deliveries {
		if d.AnchorProfileID == anchor {
			return d
		}
	}
	return platform.DeliveryStatus{}
}

// The Epic 4 exit: consent → source → governed view → segment → test webhook, where a withdrawal before
// dispatch suppresses the delivery and a replay after it no longer selects the customer.
func TestActivationDeliversOnlyWhatIsPermittedAtDispatch(t *testing.T) {
	hook := newReceiver(t)
	s := setupActivation(t, hook)
	purpose, policy := s.goldSegment()
	ada, bob := s.anchor(linkCustomerA), s.anchor(linkCustomerB)
	secret := s.destination("crm_hook", hook)
	listed := s.request("GET", "/v1/operator/destinations", "", "", viewOperator, "")
	s.expect(listed, 200)
	if strings.Contains(listed.Body.String(), secret) || strings.Contains(listed.Body.String(), "signing_secret") {
		t.Fatalf("a listing disclosed the signing secret: %s", listed.Body.String())
	}

	activation := s.activate("gold", "crm_hook")
	if activation.Queued != 2 || activation.PurposeID != purpose {
		t.Fatalf("activation %+v", activation)
	}
	if len(hook.deliveries()) != 0 {
		t.Fatal("activating sent before dispatch")
	}
	s.decideAsCustomer(linkCustomerB, purpose, policy, "withdraw")
	if n := s.dispatch(); n != 2 {
		t.Fatalf("claimed %d", n)
	}
	got := hook.deliveries()
	if len(got) != 1 {
		t.Fatalf("deliveries %+v", got)
	}
	var payload struct {
		DeliveryID      string `json:"delivery_id"`
		AnchorProfileID string `json:"anchor_profile_id"`
		Attributes      []struct {
			Path string `json:"path"`
		} `json:"attributes"`
	}
	if err := json.Unmarshal([]byte(got[0].body), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.AnchorProfileID != ada || len(payload.Attributes) != 1 || payload.Attributes[0].Path != "tier" || got[0].key != payload.DeliveryID || got[0].attempt != "1" {
		t.Fatalf("payload %s headers %+v", got[0].body, got[0])
	}
	if strings.Contains(got[0].body, "731") || strings.Contains(got[0].body, "score") {
		t.Fatalf("an attribute without an activation grant was sent: %s", got[0].body)
	}
	timestamp, mac, _ := strings.Cut(strings.TrimPrefix(got[0].signature, "t="), ",v1=")
	key, _ := hex.DecodeString(secret)
	expected := hmac.New(sha256.New, key)
	expected.Write([]byte(timestamp + "." + got[0].body))
	if !hmac.Equal([]byte(mac), []byte(hex.EncodeToString(expected.Sum(nil)))) {
		t.Fatalf("signature %q does not verify", got[0].signature)
	}

	status := s.status(activation.ID)
	if d := deliveryByAnchor(status, ada); d.State != "delivered" || len(d.Receipts) != 1 || d.Receipts[0].StatusCode == nil || *d.Receipts[0].StatusCode != 200 {
		t.Fatalf("ada %+v", d)
	}
	if d := deliveryByAnchor(status, bob); d.State != "suppressed" || d.Reason != "consent_not_permitted" || d.Receipts[0].StatusCode != nil {
		t.Fatalf("bob %+v", d)
	}
	if s.dispatch() != 0 {
		t.Fatal("a settled delivery was claimed again")
	}

	// Replaying after the withdrawal selects only Ada.
	if replay := s.activate("gold", "crm_hook"); replay.Queued != 1 || replay.Excluded.Consent != 1 {
		t.Fatalf("replay %+v", replay)
	}
	// Losing segmentation's grant on a criterion suppresses queued deliveries instead of widening.
	queued := s.activate("gold", "crm_hook")
	s.expect(s.request("POST", "/v1/operator/attributes/score/access", `{"readers":[],"sources":[]}`, "", viewOperator, `"1"`), 201)
	s.dispatch()
	if d := deliveryByAnchor(s.status(queued.ID), ada); d.State != "suppressed" || d.Reason != "segment_not_governed" {
		t.Fatalf("ungoverned %+v", d)
	}
	if _, err := s.pool.Exec(context.Background(), `UPDATE delivery_receipts SET reason='edited'`); err == nil {
		t.Fatal("a receipt was edited")
	}
}

func TestActivationRetriesAreBoundedAndFencedByConsent(t *testing.T) {
	flaky := newReceiver(t, 503, 200, 503, 503, 503, 400)
	s := setupActivation(t, flaky)
	purpose, policy := s.goldSegment()
	ada := s.anchor(linkCustomerA)
	s.decideAsCustomer(linkCustomerB, purpose, policy, "withdraw")
	s.destination("flaky", flaky)
	s.dispatcher.MaxAttempts = 3

	// A transient failure is retried with the same idempotency key.
	first := s.activate("gold", "flaky")
	s.dispatch()
	s.dispatch()
	got := flaky.deliveries()
	if d := deliveryByAnchor(s.status(first.ID), ada); d.State != "delivered" || d.Attempts != 2 || len(got) != 2 || got[0].key != got[1].key || got[1].attempt != "2" {
		t.Fatalf("retry %+v %+v", d, got)
	}

	// Retries stop at the bound.
	second := s.activate("gold", "flaky")
	for range 4 {
		s.dispatch()
	}
	if d := deliveryByAnchor(s.status(second.ID), ada); d.State != "failed" || d.Reason != "retries_exhausted" || d.Attempts != 3 || len(d.Receipts) != 3 {
		t.Fatalf("exhausted %+v", d)
	}

	// A permanent rejection is not retried.
	third := s.activate("gold", "flaky")
	s.dispatch()
	if d := deliveryByAnchor(s.status(third.ID), ada); d.State != "failed" || d.Reason != "rejected" || d.Attempts != 1 {
		t.Fatalf("rejected %+v", d)
	}

	// An unreachable receiver is a transient transport error, retried with no status code until the bound.
	// Without backoff one dispatch pass makes every attempt.
	gone := newReceiver(t)
	s.config.WebhookHosts = append(s.config.WebhookHosts, gone.host())
	s.dispatcher.Hosts = s.config.WebhookHosts
	handler, err := platform.NewAPI(platform.Store{Pool: s.pool}, s.config)
	if err != nil {
		t.Fatal(err)
	}
	s.h = handler
	s.destination("gone", gone)
	gone.Close()
	unreachable := s.activate("gold", "gone")
	s.dispatch()
	if d := deliveryByAnchor(s.status(unreachable.ID), ada); d.State != "failed" || d.Reason != "retries_exhausted" || len(d.Receipts) != 3 ||
		d.Receipts[0].Reason != "transport_error" || d.Receipts[0].StatusCode != nil {
		t.Fatalf("transport error %+v", d)
	}

	// A withdrawal between attempts blocks the queued retry.
	flaky.mu.Lock()
	flaky.statuses = []int{503}
	flaky.mu.Unlock()
	fourth := s.activate("gold", "flaky")
	s.dispatcher.Backoff = func(int32) time.Duration { return time.Hour }
	s.dispatch()
	s.decideAsCustomer(linkCustomerA, purpose, policy, "withdraw")
	before := len(flaky.deliveries())
	s.exec(`UPDATE activation_deliveries SET next_attempt_at = now() WHERE activation_id = $1`, fourth.ID)
	s.dispatch()
	if d := deliveryByAnchor(s.status(fourth.ID), ada); d.State != "suppressed" || d.Reason != "consent_not_permitted" || len(flaky.deliveries()) != before {
		t.Fatalf("fenced retry %+v", d)
	}
}

func TestDestinationsRefuseUnsafeTargets(t *testing.T) {
	hook := newReceiver(t)
	other := newReceiver(t)
	s := setupActivation(t, hook)
	s.goldSegment()
	body := func(url string) string { return fmt.Sprintf(`{"url":%q}`, url) }
	s.expect(s.request("POST", "/v1/operator/destinations/hook", body(hook.URL), "", tokenA, ""), 401)
	s.expect(s.customer("GET", "/v1/operator/destinations", "", linkCustomerA), 401)
	for _, unsafe := range []string{
		other.URL, "ftp://" + hook.host(), "http://user:pw@" + hook.host() + "/x", hook.URL + "/x?y=1", hook.URL + "/#f",
		"http://169.254.169.254/latest", "not a url",
	} {
		s.expect(s.request("POST", "/v1/operator/destinations/hook", body(unsafe), "", viewOperator, ""), 422)
	}
	s.destination("hook", hook)
	s.expect(s.request("POST", "/v1/operator/destinations/hook", body(hook.URL), "", viewOperator, ""), 409)
	s.expect(s.request("POST", segmentsPath+"/gold/activations", `{"destination":"missing"}`, "", viewOperator, ""), 422)
	s.expect(s.request("POST", segmentsPath+"/unknown/activations", `{"destination":"hook"}`, "", viewOperator, ""), 404)
	s.expect(s.request("POST", segmentsPath+"/gold/activations", `{"destination":"hook"}`, "", tokenA, ""), 401)
	s.expect(s.request("GET", "/v1/operator/activations/"+platform.UUID(), "", "", viewOperator, ""), 404)

	// A host removed from configuration after registration receives nothing.
	activation := s.activate("gold", "hook")
	s.dispatcher.Hosts = nil
	s.dispatch()
	for _, d := range s.status(activation.ID).Deliveries {
		if d.State != "failed" || d.Reason != "destination_not_allowed" {
			t.Fatalf("unlisted host %+v", d)
		}
	}
	if len(hook.deliveries()) != 0 {
		t.Fatal("an unlisted host received a delivery")
	}
}

// Redrive requeues only failed deliveries with a fresh budget, and every redriven delivery passes the
// dispatch gates again: a customer who withdrew after the failure is suppressed, not sent.
func TestRedriveRetriesFailuresThroughTheSameGates(t *testing.T) {
	hook := newReceiver(t, 503, 503, 503, 503)
	s := setupActivation(t, hook)
	purpose, policy := s.goldSegment()
	ada, bob := s.anchor(linkCustomerA), s.anchor(linkCustomerB)
	s.destination("hook", hook)
	s.dispatcher.MaxAttempts = 2
	activation := s.activate("gold", "hook")
	s.dispatch()
	status := s.status(activation.ID)
	if status.Counts["failed"] != 2 || deliveryByAnchor(status, ada).Reason != "retries_exhausted" {
		t.Fatalf("before redrive %+v", status)
	}

	s.decideAsCustomer(linkCustomerB, purpose, policy, "withdraw")
	redrive := "/v1/operator/activations/" + activation.ID + "/redrive"
	s.expect(s.request("POST", redrive, "", "", tokenA, ""), 401)
	s.expect(s.request("GET", redrive, "", "", viewOperator, ""), 405)
	s.expect(s.request("POST", "/v1/operator/activations/"+platform.UUID()+"/redrive", "", "", viewOperator, ""), 404)
	res := s.request("POST", redrive, "", "", viewOperator, "")
	s.expect(res, 200)
	if strings.TrimSpace(res.Body.String()) != `{"requeued":2}` {
		t.Fatalf("redrive %s", res.Body.String())
	}
	s.dispatch()
	status = s.status(activation.ID)
	a, b := deliveryByAnchor(status, ada), deliveryByAnchor(status, bob)
	if a.State != "delivered" || a.Attempts != 3 || a.Redrives != 1 || len(a.Receipts) != 3 {
		t.Fatalf("redriven ada %+v", a)
	}
	if b.State != "suppressed" || b.Reason != "consent_not_permitted" {
		t.Fatalf("redrive overrode a withdrawal %+v", b)
	}
	if status.Counts["delivered"] != 1 || status.Counts["suppressed"] != 1 || status.Counts["failed"] != 0 {
		t.Fatalf("counts %+v", status.Counts)
	}
	// Nothing failed remains, and suppressed deliveries are never requeued.
	if res := s.request("POST", redrive, "", "", viewOperator, ""); strings.TrimSpace(res.Body.String()) != `{"requeued":0}` {
		t.Fatalf("second redrive %s", res.Body.String())
	}
}

// A delivery rejected on its first attempt gets exactly MaxAttempts more after a redrive, not a budget
// scaled by everything it could have used before.
func TestRedriveGrantsAFreshBudget(t *testing.T) {
	hook := newReceiver(t, 400, 503, 503, 503)
	s := setupActivation(t, hook)
	purpose, policy := s.goldSegment()
	ada := s.anchor(linkCustomerA)
	s.decideAsCustomer(linkCustomerB, purpose, policy, "withdraw")
	s.destination("hook", hook)
	s.dispatcher.MaxAttempts = 2
	activation := s.activate("gold", "hook")
	s.dispatch()
	if d := deliveryByAnchor(s.status(activation.ID), ada); d.State != "failed" || d.Reason != "rejected" || d.Attempts != 1 {
		t.Fatalf("rejected %+v", d)
	}
	s.expect(s.request("POST", "/v1/operator/activations/"+activation.ID+"/redrive", "", "", viewOperator, ""), 200)
	if d := deliveryByAnchor(s.status(activation.ID), ada); d.State != "pending" || d.Reason != "" {
		t.Fatalf("a requeued delivery kept its failure reason %+v", d)
	}
	s.dispatch()
	if d := deliveryByAnchor(s.status(activation.ID), ada); d.State != "failed" || d.Reason != "retries_exhausted" || d.Attempts != 3 {
		t.Fatalf("redrive budget %+v", d)
	}
}
