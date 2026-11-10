package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/customer-fabric/customer-fabric/backend/internal/platform"
)

func (s linkingSuite) snapshot(segment string) platform.SegmentSnapshot {
	s.t.Helper()
	res := s.request("POST", segmentsPath+"/"+segment+"/snapshots", "", "", viewOperator, "")
	s.expect(res, 201)
	var snap platform.SegmentSnapshot
	if err := json.Unmarshal(res.Body.Bytes(), &snap); err != nil {
		s.t.Fatal(err)
	}
	return snap
}

func (s linkingSuite) changes(id, since string) platform.SnapshotChanges {
	s.t.Helper()
	res := s.request("GET", "/v1/operator/segment-snapshots/"+id+"/changes?since="+since, "", "", viewOperator, "")
	s.expect(res, 200)
	var c platform.SnapshotChanges
	if err := json.Unmarshal(res.Body.Bytes(), &c); err != nil {
		s.t.Fatal(err)
	}
	return c
}

func (s linkingSuite) event(record string, sequence int, set string) int {
	s.t.Helper()
	epoch, err := platform.Store{Pool: s.pool}.SourceEpoch(context.Background(), "console")
	if err != nil {
		s.t.Fatal(err)
	}
	body := fmt.Sprintf(`{"event_id":"%s-%d","type":"record.updated.v1","record":{"namespace":"crm","value":%q},"source_epoch":%d,"sequence":%d,"occurred_at":"2026-11-01T00:00:00Z","set":%s}`,
		record, sequence, record, epoch, sequence, set)
	return s.request("POST", "/v1/source-events", body, "", tokenA, "").Code
}

// Membership is a function of committed state: a replayed or out-of-order event changes no snapshot,
// and a newer event that changes a criterion shows up as exactly one customer leaving.
func TestSnapshotsAreDeterministicAndShowChanges(t *testing.T) {
	s := setupLinking(t)
	s.govern("tier", `{"type":"enum","values":["gold","silver"]}`, `{"readers":["segmentation"],"sources":["console"]}`)
	s.viewRecord(tokenA, "snap-ada", `{"tier":"gold"}`)
	s.viewRecord(tokenA, "snap-bob", `{"tier":"gold"}`)
	s.viewLink(linkIssuerA, linkCustomerA, "snap-ada")
	s.viewLink(linkIssuerA, linkCustomerB, "snap-bob")
	bob := s.anchor(linkCustomerB)
	purpose, policy := s.consentPurpose("snapshot_email")
	s.decideAsCustomer(linkCustomerA, purpose, policy, "grant")
	s.decideAsCustomer(linkCustomerB, purpose, policy, "grant")
	s.expect(s.request("POST", segmentsPath+"/gold", fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"eq","value":"gold"}]}`, purpose), "", viewOperator, ""), 201)

	first := s.snapshot("gold")
	if first.Members != 2 || first.Population != 2 || first.Revision != 1 {
		t.Fatalf("first snapshot %+v", first)
	}
	if s.event("snap-bob", 5, `{"tier":"gold"}`) != 201 || s.event("snap-bob", 5, `{"tier":"gold"}`) != 200 || s.event("snap-bob", 3, `{"tier":"silver"}`) != 201 {
		t.Fatal("event setup")
	}
	replayed := s.snapshot("gold")
	if c := s.changes(replayed.ID, first.ID); len(c.Joined) != 0 || len(c.Left) != 0 || replayed.Members != 2 {
		t.Fatalf("a replayed or stale event changed membership %+v", c)
	}

	if s.event("snap-bob", 6, `{"tier":"silver"}`) != 201 {
		t.Fatal("newer event")
	}
	after := s.snapshot("gold")
	if c := s.changes(after.ID, first.ID); len(c.Joined) != 0 || len(c.Left) != 1 || c.Left[0] != bob || c.Since.ID != first.ID {
		t.Fatalf("changes %+v", c)
	}
	if c := s.changes(first.ID, after.ID); len(c.Joined) != 1 || c.Joined[0] != bob {
		t.Fatalf("reverse changes %+v", c)
	}
	list := s.request("GET", segmentsPath+"/gold/snapshots", "", "", viewOperator, "")
	s.expect(list, 200)
	if strings.Count(list.Body.String(), `"taken_at"`) != 3 || strings.Contains(list.Body.String(), "gold\"}") {
		t.Fatalf("list %s", list.Body.String())
	}

	s.expect(s.request("POST", segmentsPath+"/silver", fmt.Sprintf(`{"purpose_id":%q,"match":"all","conditions":[{"attribute":"tier","operator":"eq","value":"silver"}]}`, purpose), "", viewOperator, ""), 201)
	other := s.snapshot("silver")
	s.expect(s.request("GET", "/v1/operator/segment-snapshots/"+other.ID+"/changes?since="+first.ID, "", "", viewOperator, ""), 422)
	s.expect(s.request("GET", "/v1/operator/segment-snapshots/"+platform.UUID()+"/changes?since="+first.ID, "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", "/v1/operator/segment-snapshots/"+first.ID+"/changes", "", "", viewOperator, ""), 400)
	s.expect(s.request("GET", "/v1/operator/segment-snapshots/"+first.ID+"/changes?since="+first.ID+"&x=1", "", "", viewOperator, ""), 400)
	s.expect(s.request("POST", segmentsPath+"/gold/snapshots", "", "", tokenA, ""), 401)
	s.expect(s.request("GET", "/v1/operator/segment-snapshots/"+first.ID+"/changes?since="+first.ID, "", "", tokenA, ""), 401)
	s.expect(s.request("POST", segmentsPath+"/unknown/snapshots", "", "", viewOperator, ""), 404)
	s.expect(s.request("GET", segmentsPath+"/unknown/snapshots", "", "", viewOperator, ""), 404)
	if _, err := s.pool.Exec(context.Background(), `DELETE FROM segment_snapshot_members`); err == nil {
		t.Fatal("a snapshot member was deleted")
	}
}
