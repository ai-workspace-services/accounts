package cutoverauth

import (
	"testing"
	"time"
)

func TestSigningBytesMatchesGatewayBatch06Golden(t *testing.T) {
	a := Authorization{SchemaVersion: 1, Kind: Kind, RequestedMode: RequestedMode, NodeID: "gw_test_01", NetworkID: "network-test", Generation: 1, SnapshotID: "snapshot_test_01", BaselineSHA256: "428299362e559ca17adeab49cfa137e8c20c8b28e5c5ad488b28f0b5aeba2794", ProjectionSHA256: "3efc8c92d20f2cf18b93980119c2cd82b67d7d32ef655e64ba55a006d8eca32d", PolicySHA256: "966ea0c4becfa9c3ed9f5120aedfed0cad64f220423acb33a16137af6c525075", Reconcile: ReconcileEvidence{Processed: 2, Completed: 2}, IssuedAt: time.Date(2026, 8, 28, 11, 59, 0, 0, time.UTC), ExpiresAt: time.Date(2026, 8, 28, 12, 10, 0, 0, time.UTC)}
	raw, err := a.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"schema_version":1,"kind":"xconnect.accounts-only-cutover-authorization","requested_mode":"accounts-only","node_id":"gw_test_01","network_id":"network-test","generation":1,"snapshot_id":"snapshot_test_01","baseline_sha256":"428299362e559ca17adeab49cfa137e8c20c8b28e5c5ad488b28f0b5aeba2794","projection_sha256":"3efc8c92d20f2cf18b93980119c2cd82b67d7d32ef655e64ba55a006d8eca32d","policy_sha256":"966ea0c4becfa9c3ed9f5120aedfed0cad64f220423acb33a16137af6c525075","reconcile":{"processed":2,"completed":2,"failed":0,"pending":0},"issued_at":"2026-08-28T11:59:00Z","expires_at":"2026-08-28T12:10:00Z"}`
	if string(raw) != want {
		t.Fatalf("canonical cutover signing bytes drifted:\n%s", raw)
	}
}
