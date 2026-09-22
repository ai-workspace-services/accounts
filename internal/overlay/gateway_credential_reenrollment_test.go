package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestGatewayInvitationRotatesOnlyMatchingGatewayCredential(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:gateway-credential-rotation?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signingKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningPrivateKey: signingKey, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	bootstrap := BootstrapConfig{
		Network: BootstrapNetwork{ID: "net-uat", DisplayName: "UAT", CIDR: "10.77.0.0/24", GatewayID: "gw-uat", GatewayWireGuardKey: key, GatewayWireGuardAddress: "10.77.0.1/32", GatewayEndpointHost: "gateway.example.test", GatewayEndpointPort: 51820, TransportServerName: "gateway.example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "owner-uat"},
		Invite:  BootstrapInvite{DeviceID: "gw-uat", Platform: "linux", Role: RoleGateway, ExpiresAt: now.Add(time.Hour)},
	}
	firstToken := newOpaqueToken("xjt_")
	_, err = service.AdminBootstrap(t.Context(), bootstrap, "https://accounts.example.test", firstToken)
	if err != nil {
		t.Fatal(err)
	}
	request := ExchangeRequest{JoinToken: firstToken, DeviceID: "gw-uat", Name: "gw-uat", Hostname: "gateway.example.test", Platform: "linux", Role: RoleGateway, WireGuardPublicKey: key}
	initial, err := service.Exchange(t.Context(), request)
	if err != nil || initial.DeviceCredential.Credential == "" {
		t.Fatalf("initial Gateway enrollment=%#v err=%v", initial, err)
	}
	secondToken := newOpaqueToken("xjt_")
	if _, err := service.AdminBootstrap(t.Context(), bootstrap, "https://accounts.example.test", secondToken); err != nil {
		t.Fatal(err)
	}
	request.JoinToken = secondToken
	rotated, err := service.Exchange(t.Context(), request)
	if err != nil || rotated.DeviceCredential.Credential == "" || rotated.DeviceCredential.Credential == initial.DeviceCredential.Credential {
		t.Fatalf("Gateway credential rotation=%#v err=%v", rotated, err)
	}
	var devices, activeCredentials, revokedCredentials int64
	if err := db.Model(&DeviceRecord{}).Where("id = ?", "gw-uat").Count(&devices).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&CredentialRecord{}).Where("device_id = ? AND revoked_at IS NULL", "gw-uat").Count(&activeCredentials).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&CredentialRecord{}).Where("device_id = ? AND revoked_at IS NOT NULL", "gw-uat").Count(&revokedCredentials).Error; err != nil {
		t.Fatal(err)
	}
	if devices != 1 || activeCredentials != 1 || revokedCredentials != 1 {
		t.Fatalf("unexpected credential projection devices=%d active=%d revoked=%d", devices, activeCredentials, revokedCredentials)
	}
	// Gateway-only recovery must not weaken One's duplicate-device protection.
	oneToken := newOpaqueToken("xjt_")
	if _, err := service.AdminBootstrap(t.Context(), BootstrapConfig{Network: bootstrap.Network, Invite: BootstrapInvite{DeviceID: "one-uat", Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, "https://accounts.example.test", oneToken); err != nil {
		t.Fatal(err)
	}
	one := ExchangeRequest{JoinToken: oneToken, DeviceID: "one-uat", Name: "one", Hostname: "one.example.test", Platform: "linux", Role: RoleOne, WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{8}, 32))}
	if _, err := service.Exchange(t.Context(), one); err != nil {
		t.Fatal(err)
	}
	oneRetryToken := newOpaqueToken("xjt_")
	if _, err := service.AdminBootstrap(t.Context(), BootstrapConfig{Network: bootstrap.Network, Invite: BootstrapInvite{DeviceID: "one-uat", Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, "https://accounts.example.test", oneRetryToken); err != nil {
		t.Fatal(err)
	}
	one.JoinToken = oneRetryToken
	if _, err := service.Exchange(t.Context(), one); err != ErrDeviceConflict {
		t.Fatalf("One re-enrollment unexpectedly accepted: %v", err)
	}
}
