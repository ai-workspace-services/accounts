package acl

import (
	"account/internal/store"
	"context"
	"testing"
)

func TestACL004TagMutationRecompilesAndAllowsOwnerGroupAdmin(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	admin := &store.User{ID: "admin-1", Name: "Admin", Email: "admin@example.com", Active: true}
	owner := &store.User{ID: "owner-1", Name: "Owner", Email: "owner@example.com", Active: true}
	if err := st.CreateUser(ctx, admin); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateUser(ctx, owner); err != nil {
		t.Fatal(err)
	}
	device := &store.OverlayDevice{ID: "dev-target", UserID: owner.ID, NetworkID: "net", Name: "target", Platform: "linux", WireGuardPublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=", WireGuardAddress: "10.0.0.2/32"}
	if err := st.UpsertOverlayDevice(ctx, device); err != nil {
		t.Fatal(err)
	}
	service, _ := NewService(st)
	source := []byte(`{"apiVersion":"overlay.xconnect.svc.plus/v1alpha1","kind":"NetworkPolicy","metadata":{"name":"tags"},"spec":{"defaultAction":"deny","groups":{"platform":{"users":["admin@example.com"]}},"tagOwners":{"tag:gateway":["group:platform"]},"rules":[{"id":"tagged","action":"accept","sources":["device:dev-target"],"destinations":["tag:gateway"],"protocols":["tcp"],"ports":[443]}]}}`)
	p, err := service.Create(ctx, "net", admin.ID, source, "application/json")
	if err != nil {
		t.Fatal(err)
	}
	p, err = service.Activate(ctx, "net", p.Revision, admin.ID)
	if err != nil || p.Generation != 1 {
		t.Fatalf("activate %#v %v", p, err)
	}
	before := p.ArtifactSHA256
	if err = service.UpdateDeviceTags(ctx, "net", device.ID, admin.ID, admin.Email, []string{"tag:gateway"}); err != nil {
		t.Fatal(err)
	}
	active, err := ResolveActive(ctx, st, "net")
	if err != nil || active.Generation != 2 || active.Digest == before {
		t.Fatalf("tag add did not recompile: %#v %v", active, err)
	}
	if err = service.UpdateDeviceTags(ctx, "net", device.ID, admin.ID, admin.Email, nil); err != nil {
		t.Fatal(err)
	}
	active, err = ResolveActive(ctx, st, "net")
	if err != nil || active.Generation != 3 {
		t.Fatalf("tag removal generation %#v %v", active, err)
	}
	if err = service.UpdateDeviceTags(ctx, "net", device.ID, admin.ID, admin.Email, []string{"tag:unknown"}); err == nil {
		t.Fatal("unknown tag accepted")
	}
	if err = service.UpdateDeviceTags(ctx, "other", device.ID, admin.ID, admin.Email, []string{"tag:gateway"}); err == nil {
		t.Fatal("cross-network tag mutation accepted")
	}
}
