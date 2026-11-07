package integration

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
	"github.com/customer-fabric/customer-fabric/backend/internal/profiles"
)

func TestOperatorProfileReadsHaveSeparateCredentials(t *testing.T) {
	s := setup(t)
	profile := s.create()
	operatorToken := strings.Repeat("o", 64)
	operatorPath := "/v1/operator/profiles"
	sourcePath := "/v1/profiles"
	operatorDetail := operatorPath + "/" + profile.ID
	s.expect(s.request(http.MethodGet, operatorPath, "", "", operatorToken, ""), 401)

	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{
		Mode:          "development",
		Tokens:        map[string]string{"console": tokenA, "importer": tokenB},
		OperatorToken: operatorToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	s.h = h
	s.expect(s.request(http.MethodGet, operatorPath, "", "", tokenA, ""), 401)
	s.expect(s.request(http.MethodGet, operatorDetail, "", "", tokenB, ""), 401)
	s.expect(s.request(http.MethodGet, sourcePath, "", "", operatorToken, ""), 401)
	s.expect(s.request(http.MethodPost, sourcePath, `{"set":{"name":"Synthetic"}}`, platform.UUID(), operatorToken, ""), 401)
	s.expect(s.request(http.MethodGet, sourcePath, "", "", tokenA, ""), 200)
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, operatorPath}, {http.MethodPatch, operatorDetail},
	} {
		w := s.request(tc.method, tc.path, "", "", operatorToken, "")
		s.expect(w, 405)
		if w.Header().Get("Allow") != "GET" {
			t.Fatal("operator profile routes must allow only GET")
		}
	}

	w := s.request(http.MethodGet, operatorPath+"?limit=1", "", "", operatorToken, "")
	s.expect(w, 200)
	var page platform.Page
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil || len(page.Items) != 1 || page.Items[0].ID != profile.ID {
		t.Fatal("operator list did not include the profile created by a source")
	}
	w = s.request(http.MethodGet, operatorDetail, "", "", operatorToken, "")
	s.expect(w, 200)
	var detail profiles.Profile
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.ID != profile.ID || w.Header().Get("ETag") != `"1"` {
		t.Fatal("operator detail did not return the profile and its current version")
	}
	if s.count("mutations") != 1 || s.count("outbox") != 1 {
		t.Fatal("operator requests changed mutation state")
	}
}
