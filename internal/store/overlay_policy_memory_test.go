package store

import (
	"context"
	"testing"
)

func TestOverlayPolicyMemoryRevisionActivationAndOwnership(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	newPolicy := func(rev uint64, owner string) *OverlayPolicyRevision {
		return &OverlayPolicyRevision{NetworkID: "net", Revision: rev, OwnerUserID: owner, Name: "p", Source: []byte(`{"kind":"NetworkPolicy"}`), Artifact: []byte(`{"network_id":"net","revision":1}`), ArtifactSHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", CompilerVersion: "v"}
	}
	first := newPolicy(1, "owner")
	if err := st.CreateOverlayPolicyRevision(ctx, first, &AuditLog{Action: AuditActionOverlayPolicyCreate}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateOverlayPolicyRevision(ctx, newPolicy(1, "owner"), &AuditLog{Action: AuditActionOverlayPolicyCreate}); err != ErrOverlayPolicyConflict {
		t.Fatalf("concurrent revision: %v", err)
	}
	active, err := st.ActivateOverlayPolicyRevision(ctx, "net", 1, "owner", &AuditLog{Action: AuditActionOverlayPolicyActivate})
	if err != nil || active.Generation != 1 {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	again, err := st.ActivateOverlayPolicyRevision(ctx, "net", 1, "owner", &AuditLog{Action: AuditActionOverlayPolicyActivate})
	if err != nil || again.Generation != 1 {
		t.Fatal("activation not idempotent")
	}
	if _, err = st.ActivateOverlayPolicyRevision(ctx, "net", 1, "admin-b", &AuditLog{Action: AuditActionOverlayPolicyActivate}); err != nil {
		t.Fatal("authorized second admin could not operate network policy")
	}
}
