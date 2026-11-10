package integration

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func (s linkingSuite) preferences(method, body, customer, etag string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/v1/customer/profile/preferences", strings.NewReader(body))
	req.Header.Set("X-Session-Token", customer)
	req.Header.Set("Content-Type", "application/json")
	if etag != "" {
		req.Header.Set("If-Match", etag)
	}
	res := httptest.NewRecorder()
	s.h.ServeHTTP(res, req)
	return res
}

func decodePreferences(t *testing.T, res *httptest.ResponseRecorder) platform.CustomerPreferences {
	t.Helper()
	var p platform.CustomerPreferences
	if err := json.Unmarshal(res.Body.Bytes(), &p); err != nil {
		t.Fatalf("%d %s", res.Code, res.Body.String())
	}
	return p
}

// A customer owns display name, language and timezone: separate from source attributes and Kratos traits,
// versioned against concurrent edits, and logged by field without values.
func TestCustomerOwnsPreferences(t *testing.T) {
	s := setupLinking(t)
	empty := s.preferences("GET", "", linkCustomerA, "")
	s.expect(empty, 200)
	if p := decodePreferences(t, empty); p.Version != 0 || p.DisplayName != nil || empty.Header().Get("ETag") != "" {
		t.Fatalf("before any write %s", empty.Body.String())
	}

	first := s.preferences("PUT", `{"display_name":"  Zoë Weiß ","preferred_language":"de-de","timezone":"Europe/Berlin"}`, linkCustomerA, "")
	s.expect(first, 200)
	p := decodePreferences(t, first)
	if p.Version != 1 || *p.DisplayName != "Zoë Weiß" || *p.PreferredLanguage != "de-DE" || first.Header().Get("ETag") != `"1"` {
		t.Fatalf("first write %s", first.Body.String())
	}
	s.expect(s.preferences("PUT", `{"timezone":"UTC"}`, linkCustomerA, ""), 428)
	s.expect(s.preferences("PUT", `{"timezone":"UTC"}`, linkCustomerA, `"2"`), 412)
	second := s.preferences("PUT", `{"display_name":"Zoë Weiß","preferred_language":"de-DE","timezone":"America/New_York"}`, linkCustomerA, `"1"`)
	if p := decodePreferences(t, second); p.Version != 2 || len(p.Changes) != 2 || strings.Join(p.Changes[0].Fields, ",") != "timezone" {
		t.Fatalf("second write %s", second.Body.String())
	}
	if unchanged := decodePreferences(t, s.preferences("PUT", `{"display_name":"Zoë Weiß","preferred_language":"de-DE","timezone":"America/New_York"}`, linkCustomerA, `"2"`)); unchanged.Version != 2 {
		t.Fatalf("an unchanged document created a version: %d", unchanged.Version)
	}
	for _, bad := range []string{`{"timezone":"Mars/Olympus"}`, `{"preferred_language":"klingon please"}`, `{"display_name":"a\nb"}`} {
		s.expect(s.preferences("PUT", bad, linkCustomerA, `"2"`), 422)
	}
	s.expect(s.preferences("PUT", `{"display_name":"x","email":"x@example.com"}`, linkCustomerA, `"2"`), 400)

	// Another customer sees only their own, and other credentials cannot reach these fields.
	if other := decodePreferences(t, s.preferences("GET", "", linkCustomerB, "")); other.Version != 0 {
		t.Fatalf("customer B saw A's preferences %+v", other)
	}
	s.expect(s.request("GET", "/v1/customer/profile/preferences", "", "", viewOperator, ""), 401)
	s.expect(s.request("PUT", "/v1/customer/profile/preferences", `{"display_name":"x"}`, "", tokenA, ""), 401)

	// The fields live apart from the profile's source-owned attributes and its provenance.
	var attributes string
	if err := s.pool.QueryRow(context.Background(), `SELECT attributes::text FROM profiles WHERE id=$1`, s.anchor(linkCustomerA)).Scan(&attributes); err != nil || attributes != "{}" {
		t.Fatalf("anchor attributes %q %v", attributes, err)
	}
	if strings.Contains(second.Body.String(), "Europe/Berlin") || !strings.Contains(second.Body.String(), `"fields":["timezone"]`) {
		t.Fatalf("change log %s", second.Body.String())
	}
	s.exec(`UPDATE profiles SET lifecycle_state='erasure_pending' WHERE id=$1`, s.anchor(linkCustomerA))
	s.expect(s.preferences("PUT", `{"timezone":"UTC"}`, linkCustomerA, `"2"`), 409)
	if _, err := s.pool.Exec(context.Background(), `UPDATE customer_preference_changes SET fields='{timezone}'`); err == nil {
		t.Fatal("a change record was edited")
	}
}
