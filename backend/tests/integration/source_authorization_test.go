package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func TestSourceAuthorizationSurvivesOutboxDeletionAndRotation(t *testing.T) {
	s := setup(t)
	profile := s.create()
	if _, err := s.pool.Exec(context.Background(), "DELETE FROM outbox WHERE profile_id=$1", profile.ID); err != nil {
		t.Fatal(err)
	}
	rotated := strings.Repeat("r", 64)
	h, err := platform.NewAPI(platform.Store{Pool: s.pool}, platform.Config{
		Mode:            "development",
		Tokens:          map[string]string{"console": rotated, "importer": tokenB},
		OperatorKratosPublicURL: newOperatorIssuer(t),
		KratosPublicURL:         "http://127.0.0.1:1",
	})
	if err != nil {
		t.Fatal(err)
	}
	s.h = h
	path := "/v1/profiles/" + profile.ID
	payload := `{"set":{"name":"Updated synthetic profile"}}`
	s.expect(s.request("PATCH", path, payload, platform.UUID(), tokenB, `"1"`), 403)
	s.expect(s.request("PATCH", path, payload, platform.UUID(), tokenA, `"1"`), 401)
	s.expect(s.request("PATCH", path, payload, platform.UUID(), rotated, `"1"`), 200)
}
