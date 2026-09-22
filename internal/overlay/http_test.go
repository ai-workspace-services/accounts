package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newOverlayHTTPTest(t *testing.T) (*Service, *gin.Engine, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:overlay-http-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-1", SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	gatewayKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	joinToken, err := service.Seed(t.Context(), BootstrapConfig{Network: BootstrapNetwork{ID: "sit-private", DisplayName: "SIT private", CIDR: "10.77.0.0/29", GatewayID: "gw-sit-1", GatewayWireGuardKey: gatewayKey, GatewayWireGuardAddress: "10.77.0.1/32", GatewayEndpointHost: "gw.example.test", GatewayEndpointPort: 51820, TransportServerName: "gw.example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "11111111-2222-4333-8444-555555555555"}, Invite: BootstrapInvite{Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, "")
	if err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	handler := NewHTTPHandler(service)
	router.POST("/api/overlay/v1/registrations", handler.Register)
	router.POST("/api/overlay/v1/registrations/:registrationID/exchange", handler.RegistrationExchange)
	router.POST("/api/overlay/v1/join-tokens/exchange", handler.Exchange)
	router.POST("/api/overlay/v1/device/session", handler.MintSession)
	router.GET("/api/overlay/v1/enrollment/signed-config", handler.EnrollmentConfig)
	router.POST("/api/overlay/v1/enrollment/signed-config/:generation/ack", handler.AckEnrollment)
	router.GET("/api/overlay/v1/enrollment/policy-artifacts/:generation/:digest", handler.PolicyArtifact)
	router.GET("/api/overlay/v1/gateway/signed-config", handler.GatewayConfig)
	return service, router, joinToken
}

func newRegistrationTestService(t *testing.T) (*Service, *gorm.DB, *time.Time, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:registration-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningPrivateKey: signer, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	legacyToken := newOpaqueToken("xjt_")
	_, err = service.Seed(t.Context(), BootstrapConfig{Network: BootstrapNetwork{ID: "network-a", DisplayName: "Network A", CIDR: "10.77.0.0/24", GatewayID: "gateway-a", GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32)), GatewayWireGuardAddress: "10.77.0.1/32", GatewayEndpointHost: "gateway.example.test", GatewayEndpointPort: 51820, TransportServerName: "gateway.example.test", TransportPort: 443, TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: "11111111-2222-4333-8444-555555555555"}, Invite: BootstrapInvite{Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)}}, legacyToken)
	if err != nil {
		t.Fatal(err)
	}
	return service, db, &now, legacyToken
}

func registrationRequest(id string, keyByte byte) RegistrationRequest {
	return RegistrationRequest{NetworkID: "network-a", DeviceID: id, Name: "One " + id, Hostname: id + ".example.test", Platform: "linux", WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{keyByte}, 32))}
}

func TestRegistrationPendingApprovalAndExchangeAreSeparated(t *testing.T) {
	service, db, _, legacyToken := newRegistrationTestService(t)
	request := registrationRequest("one-self-register", 9)
	registered, err := service.Register(t.Context(), request)
	if err != nil || registered.Status != RegistrationStatusPending || !validOpaque(registered.RegistrationToken, "xrt_") || registered.Interval != 5 {
		t.Fatalf("register=%#v err=%v", registered, err)
	}
	var record RegistrationRecord
	if err := db.Where("id = ?", registered.RegistrationID).First(&record).Error; err != nil || record.TokenHash != HashSecret(registered.RegistrationToken) || record.TokenHash == registered.RegistrationToken || record.OwnerUserID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("unsafe or unbound registration storage: %#v err=%v", record, err)
	}
	for _, model := range []any{&DeviceRecord{}, &CredentialRecord{}, &EnrollmentRecord{}} {
		var count int64
		if err := db.Model(model).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("pending registration changed protected state model=%T count=%d err=%v", model, count, err)
		}
	}
	var network NetworkRecord
	if err := db.Where("id = ?", request.NetworkID).First(&network).Error; err != nil || network.ConfigGeneration != 1 {
		t.Fatalf("pending registration changed network generation: %#v err=%v", network, err)
	}
	if _, pending, err := service.ExchangeRegistration(t.Context(), registered.RegistrationID, registered.RegistrationToken); !errors.Is(err, ErrRegistrationPending) || pending.Status != RegistrationStatusPending || pending.ExpiresAt != registered.ExpiresAt {
		t.Fatalf("pending exchange=%#v err=%v", pending, err)
	}

	// A registration token is not an invitation. The legacy path must reject it,
	// while its own invitation remains functional.
	if _, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: registered.RegistrationToken, DeviceID: "wrong-path-raw", Platform: "linux", WireGuardPublicKey: registrationRequest("unused", 10).WireGuardPublicKey, Role: RoleOne}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("raw pending registration token was accepted as invite: %v", err)
	}
	inviteShapedRegistrationToken := "xjt_" + strings.TrimPrefix(registered.RegistrationToken, "xrt_")
	if _, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: inviteShapedRegistrationToken, DeviceID: "wrong-path", Platform: "linux", WireGuardPublicKey: registrationRequest("unused", 10).WireGuardPublicKey, Role: RoleOne}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("registration token was accepted as invite: %v", err)
	}
	legacy, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: legacyToken, DeviceID: "legacy-invite", Platform: "linux", WireGuardPublicKey: registrationRequest("unused", 10).WireGuardPublicKey, Role: RoleOne})
	if err != nil || legacy.Device.ID != "legacy-invite" {
		t.Fatalf("legacy invitation no longer works: %#v err=%v", legacy, err)
	}

	approved, err := service.AdminApproveRegistration(t.Context(), record.OwnerUserID, registered.RegistrationID, request.NetworkID)
	if err != nil || approved.Status != RegistrationStatusApproved || approved.WireGuardPublicKeyFingerprint != fingerprintWireGuardPublicKey(bytes.Repeat([]byte{9}, 32)) {
		t.Fatalf("approve=%#v err=%v", approved, err)
	}
	if _, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: registered.RegistrationToken, DeviceID: "wrong-path-approved-raw", Platform: "linux", WireGuardPublicKey: registrationRequest("unused", 10).WireGuardPublicKey, Role: RoleOne}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("raw approved registration token was accepted as invite: %v", err)
	}
	if _, err := service.Exchange(t.Context(), ExchangeRequest{JoinToken: inviteShapedRegistrationToken, DeviceID: "wrong-path-approved", Platform: "linux", WireGuardPublicKey: registrationRequest("unused", 10).WireGuardPublicKey, Role: RoleOne}); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("approved registration token was accepted as invite: %v", err)
	}
	exchange, _, err := service.ExchangeRegistration(t.Context(), registered.RegistrationID, registered.RegistrationToken)
	if err != nil || exchange.Device.ID != request.DeviceID || exchange.DeviceCredential.Credential == "" || exchange.EnrollmentToken == "" {
		t.Fatalf("approved exchange=%#v err=%v", exchange, err)
	}
	if _, _, err := service.ExchangeRegistration(t.Context(), registered.RegistrationID, registered.RegistrationToken); !errors.Is(err, ErrRegistrationConsumed) {
		t.Fatalf("registration replay accepted: %v", err)
	}
	var devices, credentials, enrollments int64
	_ = db.Model(&DeviceRecord{}).Where("id = ?", request.DeviceID).Count(&devices).Error
	_ = db.Model(&CredentialRecord{}).Where("device_id = ?", request.DeviceID).Count(&credentials).Error
	_ = db.Model(&EnrollmentRecord{}).Where("device_id = ?", request.DeviceID).Count(&enrollments).Error
	if devices != 1 || credentials != 1 || enrollments != 1 {
		t.Fatalf("exchange was not exactly once: devices=%d credentials=%d enrollments=%d", devices, credentials, enrollments)
	}
	if _, err := service.Register(t.Context(), request); !errors.Is(err, ErrDeviceConflict) {
		t.Fatalf("existing device identity accepted for new pending registration: %v", err)
	}
}

func TestRegistrationExpiryOwnerIsolationAndConcurrentTransitions(t *testing.T) {
	service, db, now, _ := newRegistrationTestService(t)
	request := registrationRequest("one-expiring", 11)
	registered, err := service.Register(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	*now = registered.ExpiresAt
	if _, _, err := service.ExchangeRegistration(t.Context(), registered.RegistrationID, registered.RegistrationToken); !errors.Is(err, ErrRegistrationExpired) {
		t.Fatalf("expiry boundary error=%v", err)
	}
	var expired RegistrationRecord
	if err := db.Where("id = ?", registered.RegistrationID).First(&expired).Error; err != nil || expired.Status != RegistrationStatusExpired {
		t.Fatalf("expiry did not persist: %#v err=%v", expired, err)
	}

	*now = (*now).Add(time.Minute)
	isolated, err := service.Register(t.Context(), registrationRequest("one-owner-change", 12))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&NetworkRecord{}).Where("id = ?", "network-a").Update("owner_user_id", "other-owner").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdminApproveRegistration(t.Context(), "11111111-2222-4333-8444-555555555555", isolated.RegistrationID, "network-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old owner retained authority after network owner change: %v", err)
	}
	if _, err := service.AdminApproveRegistration(t.Context(), "other-owner", isolated.RegistrationID, "network-a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("new owner gained authority over old registration: %v", err)
	}
	if page, err := service.AdminRegistrations(t.Context(), "11111111-2222-4333-8444-555555555555", ""); err != nil || len(page.Registrations) != 0 {
		t.Fatalf("old owner retained registration metadata after network transfer: %#v err=%v", page, err)
	}
	if page, err := service.AdminRegistrations(t.Context(), "other-owner", ""); err != nil || len(page.Registrations) != 0 {
		t.Fatalf("new owner inherited registration metadata after network transfer: %#v err=%v", page, err)
	}
	if err := db.Model(&RegistrationRecord{}).Where("id = ?", isolated.RegistrationID).Updates(map[string]any{"status": RegistrationStatusApproved, "approved_at": *now}).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ExchangeRegistration(t.Context(), isolated.RegistrationID, isolated.RegistrationToken); !errors.Is(err, ErrRegistrationNotAvailable) {
		t.Fatalf("owner-changed registration granted a device: %v", err)
	}

	if err := db.Model(&NetworkRecord{}).Where("id = ?", "network-a").Update("owner_user_id", "11111111-2222-4333-8444-555555555555").Error; err != nil {
		t.Fatal(err)
	}
	race, err := service.Register(t.Context(), registrationRequest("one-transition-race", 13))
	if err != nil {
		t.Fatal(err)
	}
	database, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	var wait sync.WaitGroup
	errorsOut := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		_, err := service.AdminApproveRegistration(t.Context(), "11111111-2222-4333-8444-555555555555", race.RegistrationID, "network-a")
		errorsOut <- err
	}()
	go func() {
		defer wait.Done()
		_, err := service.AdminRejectRegistration(t.Context(), "11111111-2222-4333-8444-555555555555", race.RegistrationID)
		errorsOut <- err
	}()
	wait.Wait()
	close(errorsOut)
	success, stale := 0, 0
	for err := range errorsOut {
		if err == nil {
			success++
		} else if errors.Is(err, ErrRegistrationNotPending) {
			stale++
		} else {
			t.Fatalf("transition race error=%v", err)
		}
	}
	if success != 1 || stale != 1 {
		t.Fatalf("transitions were not atomic: success=%d stale=%d", success, stale)
	}
}

func TestRegistrationTerminalStateAndRateLimits(t *testing.T) {
	service, _, now, _ := newRegistrationTestService(t)
	owner := "11111111-2222-4333-8444-555555555555"

	rejected, err := service.Register(t.Context(), registrationRequest("one-rejected", 21))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdminRejectRegistration(t.Context(), owner, rejected.RegistrationID); err != nil {
		t.Fatal(err)
	}
	*now = rejected.ExpiresAt.Add(time.Hour)
	if _, _, err := service.ExchangeRegistration(t.Context(), rejected.RegistrationID, rejected.RegistrationToken); !errors.Is(err, ErrRegistrationRejected) {
		t.Fatalf("rejected terminal state changed after expiry: %v", err)
	}

	consumed, err := service.Register(t.Context(), registrationRequest("one-consumed", 22))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdminApproveRegistration(t.Context(), owner, consumed.RegistrationID, "network-a"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := service.ExchangeRegistration(t.Context(), consumed.RegistrationID, consumed.RegistrationToken); err != nil {
		t.Fatal(err)
	}
	*now = consumed.ExpiresAt.Add(time.Hour)
	if _, _, err := service.ExchangeRegistration(t.Context(), consumed.RegistrationID, consumed.RegistrationToken); !errors.Is(err, ErrRegistrationConsumed) {
		t.Fatalf("consumed terminal state changed after expiry: %v", err)
	}

	approved, err := service.Register(t.Context(), registrationRequest("one-approved-expiry", 23))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdminApproveRegistration(t.Context(), owner, approved.RegistrationID, "network-a"); err != nil {
		t.Fatal(err)
	}
	*now = approved.ExpiresAt
	if _, _, err := service.ExchangeRegistration(t.Context(), approved.RegistrationID, approved.RegistrationToken); !errors.Is(err, ErrRegistrationExpired) {
		t.Fatalf("approved expiry boundary error=%v", err)
	}

	limitService, _, limitNow, _ := newRegistrationTestService(t)
	// Advance past the 15-minute rate window between batches. Every old
	// pending record has also expired, so this exercises the independent,
	// durable daily creation budget rather than the pending cap.
	for batch := 0; batch < 5; batch++ {
		*limitNow = limitNow.Add(registrationCreateWindow + time.Second)
		for item := 0; item < registrationCreateWindowLimit; item++ {
			id := fmt.Sprintf("one-rate-%d-%d", batch, item)
			if _, err := limitService.Register(t.Context(), registrationRequest(id, byte(40+batch*registrationCreateWindowLimit+item))); err != nil {
				t.Fatalf("rate batch=%d item=%d err=%v", batch, item, err)
			}
		}
	}
	*limitNow = limitNow.Add(registrationCreateWindow + time.Second)
	if _, err := limitService.Register(t.Context(), registrationRequest("one-daily-overflow", 200)); !errors.Is(err, ErrRegistrationLimited) {
		t.Fatalf("daily durable rate limit was bypassed: %v", err)
	}
}

func TestRegistrationCanonicalizesDeclaredWireGuardPublicKey(t *testing.T) {
	service, db, _, _ := newRegistrationTestService(t)
	key := bytes.Repeat([]byte{31}, 32)
	request := registrationRequest("one-canonical-key", 31)
	request.WireGuardPublicKey = base64.RawStdEncoding.EncodeToString(key)
	registered, err := service.Register(t.Context(), request)
	if err != nil {
		t.Fatal(err)
	}
	var record RegistrationRecord
	if err := db.Where("id = ?", registered.RegistrationID).First(&record).Error; err != nil {
		t.Fatal(err)
	}
	if record.WireGuardPublicKey != base64.StdEncoding.EncodeToString(key) || record.WireGuardPublicKeyFingerprint != fingerprintWireGuardPublicKey(key) {
		t.Fatalf("public key was not canonicalized: %#v", record)
	}
}

func TestRegistrationExchangeConsumesOnlyOnce(t *testing.T) {
	service, db, _, _ := newRegistrationTestService(t)
	registered, err := service.Register(t.Context(), registrationRequest("one-consume-race", 24))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdminApproveRegistration(t.Context(), "11111111-2222-4333-8444-555555555555", registered.RegistrationID, "network-a"); err != nil {
		t.Fatal(err)
	}
	database, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	var wait sync.WaitGroup
	errorsOut := make(chan error, 4)
	for range 4 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _, err := service.ExchangeRegistration(t.Context(), registered.RegistrationID, registered.RegistrationToken)
			errorsOut <- err
		}()
	}
	wait.Wait()
	close(errorsOut)
	successes, consumed := 0, 0
	for err := range errorsOut {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrRegistrationConsumed) {
			consumed++
		} else {
			t.Fatalf("parallel exchange error=%v", err)
		}
	}
	if successes != 1 || consumed != 3 {
		t.Fatalf("exchange was replayable: success=%d consumed=%d", successes, consumed)
	}
}

func TestRegistrationHTTPContract(t *testing.T) {
	service, router, _ := newOverlayHTTPTest(t)
	payload, err := json.Marshal(RegistrationRequest{NetworkID: "sit-private", DeviceID: "one-http-registration", Name: "HTTP One", Hostname: "one-http", Platform: "linux", WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{26}, 32))})
	if err != nil {
		t.Fatal(err)
	}
	registerRequest := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/registrations", bytes.NewReader(payload))
	registerResponse := httptest.NewRecorder()
	router.ServeHTTP(registerResponse, registerRequest)
	if registerResponse.Code != http.StatusCreated || registerResponse.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("register status=%d headers=%v body=%s", registerResponse.Code, registerResponse.Header(), registerResponse.Body.String())
	}
	var registered RegistrationResponse
	if err := json.Unmarshal(registerResponse.Body.Bytes(), &registered); err != nil || registered.RegistrationToken == "" || registered.Status != RegistrationStatusPending {
		t.Fatalf("registration response=%s decoded=%#v err=%v", registerResponse.Body.String(), registered, err)
	}
	missingToken := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/registrations/"+registered.RegistrationID+"/exchange", nil)
	missingTokenResponse := httptest.NewRecorder()
	router.ServeHTTP(missingTokenResponse, missingToken)
	if missingTokenResponse.Code != http.StatusUnauthorized {
		t.Fatalf("missing registration token status=%d body=%s", missingTokenResponse.Code, missingTokenResponse.Body.String())
	}
	pending := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/registrations/"+registered.RegistrationID+"/exchange", nil)
	pending.Header.Set("Authorization", "Bearer "+registered.RegistrationToken)
	pendingResponse := httptest.NewRecorder()
	router.ServeHTTP(pendingResponse, pending)
	if pendingResponse.Code != http.StatusAccepted || pendingResponse.Header().Get("Cache-Control") != "no-store" || !strings.Contains(pendingResponse.Body.String(), `"status":"pending"`) {
		t.Fatalf("pending status=%d headers=%v body=%s", pendingResponse.Code, pendingResponse.Header(), pendingResponse.Body.String())
	}
	if _, err := service.AdminApproveRegistration(t.Context(), "11111111-2222-4333-8444-555555555555", registered.RegistrationID, "sit-private"); err != nil {
		t.Fatal(err)
	}
	legacy := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens/exchange", bytes.NewBufferString(`{"join_token":"`+registered.RegistrationToken+`","device_id":"wrong-endpoint","platform":"linux","wireguard_public_key":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{27}, 32))+`"}`))
	legacyResponse := httptest.NewRecorder()
	router.ServeHTTP(legacyResponse, legacy)
	if legacyResponse.Code != http.StatusUnauthorized {
		t.Fatalf("legacy invite endpoint accepted xrt: status=%d body=%s", legacyResponse.Code, legacyResponse.Body.String())
	}
	exchangeRequest := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/registrations/"+registered.RegistrationID+"/exchange", nil)
	exchangeRequest.Header.Set("Authorization", "Bearer "+registered.RegistrationToken)
	exchangeResponse := httptest.NewRecorder()
	router.ServeHTTP(exchangeResponse, exchangeRequest)
	if exchangeResponse.Code != http.StatusOK || exchangeResponse.Header().Get("Cache-Control") != "no-store" || !strings.Contains(exchangeResponse.Body.String(), `"id":"one-http-registration"`) {
		t.Fatalf("approved exchange status=%d headers=%v body=%s", exchangeResponse.Code, exchangeResponse.Header(), exchangeResponse.Body.String())
	}
}

func TestAdminRegistrationsPaginationAndOrphanNetwork(t *testing.T) {
	service, db, now, _ := newRegistrationTestService(t)
	owner := "11111111-2222-4333-8444-555555555555"
	if err := db.Model(&NetworkRecord{}).Where("id = ?", "network-a").Update("owner_user_id", "").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := service.Register(t.Context(), registrationRequest("one-orphan", 25)); !errors.Is(err, ErrRegistrationNotAvailable) {
		t.Fatalf("ownerless network accepted registration: %v", err)
	}
	if err := db.Model(&NetworkRecord{}).Where("id = ?", "network-a").Update("owner_user_id", owner).Error; err != nil {
		t.Fatal(err)
	}
	for index := 0; index < registrationAdminPageSize+1; index++ {
		created := now.Add(time.Duration(index) * time.Second)
		record := RegistrationRecord{ID: fmt.Sprintf("xreg-page-%03d", index), NetworkID: "network-a", OwnerUserID: owner, DeviceID: fmt.Sprintf("one-page-%03d", index), Platform: "linux", WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{byte(index)}, 32)), WireGuardPublicKeyFingerprint: fmt.Sprintf("fingerprint-%03d", index), TokenHash: fmt.Sprintf("token-hash-%03d", index), Status: RegistrationStatusRejected, ExpiresAt: created.Add(registrationTTL), RejectedAt: &created, CreatedAt: created, UpdatedAt: created}
		if err := db.Create(&record).Error; err != nil {
			t.Fatal(err)
		}
	}
	page, err := service.AdminRegistrations(t.Context(), owner, "")
	if err != nil || len(page.Registrations) != registrationAdminPageSize || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("first registration page=%#v err=%v", page, err)
	}
	next, err := service.AdminRegistrations(t.Context(), owner, page.NextCursor)
	if err != nil || len(next.Registrations) != 1 || next.HasMore {
		t.Fatalf("second registration page=%#v err=%v", next, err)
	}
}

func TestOverlayLifecyclePersistsHashesAndSignsConfig(t *testing.T) {
	service, router, joinToken := newOverlayHTTPTest(t)
	publicKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))
	payload := `{"join_token":"` + joinToken + `","device_id":"one-laptop","name":"Laptop","platform":"linux","hostname":"laptop","wireguard_public_key":"` + publicKey + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens/exchange", bytes.NewBufferString(payload))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("exchange status=%d headers=%v body=%s", resp.Code, resp.Header(), resp.Body.String())
	}
	var exchange ExchangeResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &exchange); err != nil {
		t.Fatal(err)
	}
	if exchange.Device.Role != "" || exchange.Device.WireGuardAddress != "10.77.0.2/32" {
		t.Fatalf("unexpected One device response: %#v", exchange.Device)
	}
	var exchangePayload map[string]json.RawMessage
	if err := json.Unmarshal(resp.Body.Bytes(), &exchangePayload); err != nil {
		t.Fatal(err)
	}
	var exchangeDevicePayload map[string]json.RawMessage
	if err := json.Unmarshal(exchangePayload["device"], &exchangeDevicePayload); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"role", "status", "connection_status"} {
		if _, ok := exchangeDevicePayload[field]; ok {
			t.Fatalf("strict exchange Device contract unexpectedly exposed admin field %q: %s", field, exchangeDevicePayload[field])
		}
	}
	var storedDevice DeviceRecord
	if err := service.repo.DB.First(&storedDevice, "id = ?", exchange.Device.ID).Error; err != nil || storedDevice.UserUUID != storedDevice.UserID || storedDevice.UserID != "11111111-2222-4333-8444-555555555555" {
		t.Fatalf("device owner compatibility columns diverged: %#v err=%v", storedDevice, err)
	}
	var credentials []CredentialRecord
	if err := service.repo.DB.Find(&credentials).Error; err != nil || len(credentials) != 1 || credentials[0].TokenHash == exchange.DeviceCredential.Credential {
		t.Fatalf("credential storage is unsafe: err=%v records=%#v", err, credentials)
	}
	credentialRawID := strings.TrimPrefix(exchange.DeviceCredential.CredentialID, "xdcid_")
	if !strings.HasPrefix(exchange.DeviceCredential.Credential, "xdc_"+credentialRawID+".") {
		t.Fatalf("device credential is not bound to credential_id")
	}
	var invites []InviteRecord
	if err := service.repo.DB.Find(&invites).Error; err != nil || len(invites) != 1 || invites[0].TokenHash == joinToken || invites[0].RemainingUses != 0 {
		t.Fatalf("invite storage is unsafe: err=%v records=%#v", err, invites)
	}

	sessionBody := `{"client_nonce":"00000000-0000-4000-8000-000000000001"}`
	sessionReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/device/session", bytes.NewBufferString(sessionBody))
	sessionReq.Header.Set("Authorization", "Device "+exchange.DeviceCredential.Credential)
	sessionResp := httptest.NewRecorder()
	router.ServeHTTP(sessionResp, sessionReq)
	if sessionResp.Code != http.StatusOK {
		t.Fatalf("session status=%d body=%s", sessionResp.Code, sessionResp.Body.String())
	}
	var session DeviceSessionResponse
	if err := json.Unmarshal(sessionResp.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}

	configReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config?device_id=one-laptop&network_id=sit-private", nil)
	configReq.Header.Set("Authorization", "Bearer "+session.EnrollmentToken)
	configResp := httptest.NewRecorder()
	router.ServeHTTP(configResp, configReq)
	if configResp.Code != http.StatusOK || configResp.Header().Get("Cache-Control") != "private, no-store" || configResp.Header().Get("ETag") == "" {
		t.Fatalf("config status=%d headers=%v body=%s", configResp.Code, configResp.Header(), configResp.Body.String())
	}
	var config SignedConfig
	if err := json.Unmarshal(configResp.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	unsigned, err := signingBytes(config)
	if err != nil || !ed25519.Verify(service.privateKey.Public().(ed25519.PublicKey), unsigned, mustDecodeBase64(t, config.Signature.Value)) {
		t.Fatalf("signed config verification failed: err=%v config=%#v", err, config)
	}
	if config.Transport.Kind != TransportVLESSXHTTP || config.Transport.Loopback != (Endpoint{Host: "127.0.0.1", Port: 51830}) || config.Transport.Remote.Port != 443 || config.Transport.Path != DefaultTransportPath || config.Transport.Mode != DefaultTransportMode || config.WireGuard.Peers[0].Endpoint.Port != 51830 {
		t.Fatalf("unexpected XHTTP One transport contract: %#v", config)
	}
	v2Req := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config?device_id=one-laptop&network_id=sit-private", nil)
	v2Req.Header.Set("Authorization", "Bearer "+session.EnrollmentToken)
	v2Req.Header.Set("Accept", SignedConfigV2MediaType)
	v2Resp := httptest.NewRecorder()
	router.ServeHTTP(v2Resp, v2Req)
	if v2Resp.Code != http.StatusOK || v2Resp.Header().Get("Content-Type") != SignedConfigV2MediaType {
		t.Fatalf("v2 config status=%d content-type=%q body=%s", v2Resp.Code, v2Resp.Header().Get("Content-Type"), v2Resp.Body.String())
	}
	var v2 SignedConfig
	if err := json.Unmarshal(v2Resp.Body.Bytes(), &v2); err != nil || v2.SchemaVersion != 2 || v2.Policy == nil {
		t.Fatalf("invalid v2 config: err=%v config=%#v", err, v2)
	}
	policyReq := httptest.NewRequest(http.MethodGet, v2.Policy.Path, nil)
	policyReq.Header.Set("Authorization", "Bearer "+session.EnrollmentToken)
	policyResp := httptest.NewRecorder()
	router.ServeHTTP(policyResp, policyReq)
	if policyResp.Code != http.StatusOK || policyResp.Header().Get("Content-Type") != PolicyMediaType {
		t.Fatalf("policy status=%d content-type=%q body=%s", policyResp.Code, policyResp.Header().Get("Content-Type"), policyResp.Body.String())
	}

	ackBody := `{"config_id":"` + config.ConfigID + `","device_id":"one-laptop","applied_at":"2026-09-07T12:00:00Z"}`
	ackReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/overlay/v1/enrollment/signed-config/%d/ack", config.Generation), bytes.NewBufferString(ackBody))
	ackReq.Header.Set("Authorization", "Bearer "+session.EnrollmentToken)
	ackResp := httptest.NewRecorder()
	router.ServeHTTP(ackResp, ackReq)
	if ackResp.Code != http.StatusOK || !bytes.Contains(ackResp.Body.Bytes(), []byte(`"acked":true`)) {
		t.Fatalf("ack status=%d body=%s", ackResp.Code, ackResp.Body.String())
	}
	var acknowledgedDevice DeviceRecord
	if err := service.repo.DB.Where("id = ?", "one-laptop").First(&acknowledgedDevice).Error; err != nil {
		t.Fatal(err)
	}
	if acknowledgedDevice.LastSeenAt == nil {
		t.Fatal("successful signed-config ACK did not update device activity")
	}

	replayReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens/exchange", bytes.NewBufferString(payload))
	replayResp := httptest.NewRecorder()
	router.ServeHTTP(replayResp, replayReq)
	if replayResp.Code != http.StatusUnauthorized {
		t.Fatalf("expected one-use invite rejection, got %d: %s", replayResp.Code, replayResp.Body.String())
	}
}

func TestGatewayRoleUsesCentralSignedSnapshot(t *testing.T) {
	t.Setenv("XCONNECT_GATEWAY_XRAY_FRONTEND", GatewayFrontendCaddyUnixH2C)
	t.Setenv("XCONNECT_GATEWAY_XRAY_LISTEN_SOCKET", DefaultGatewayListenSocket)
	service, router, _ := newOverlayHTTPTest(t)
	joinToken, err := service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{ID: "uat-private", DisplayName: "UAT private", CIDR: "10.88.0.0/29", GatewayID: "gw-uat-1", GatewayWireGuardKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)), GatewayWireGuardAddress: "10.88.0.1/32", GatewayEndpointHost: "gw.uat.test", GatewayEndpointPort: 51820, TransportServerName: "gw.uat.test", TransportPort: 443, TransportAuthID: "22222222-2222-2222-2222-222222222222"},
		Invite:  BootstrapInvite{Role: RoleGateway, Platform: "linux", ExpiresAt: time.Now().UTC().Add(time.Hour)},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"join_token":"` + joinToken + `","device_id":"gw-uat-1","platform":"linux","role":"gateway","wireguard_public_key":"` + base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens/exchange", bytes.NewBufferString(payload))
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK {
		t.Fatalf("gateway exchange status=%d body=%s", resp.Code, resp.Body.String())
	}
	var exchange ExchangeResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &exchange); err != nil {
		t.Fatal(err)
	}
	if exchange.Device.WireGuardAddress != "10.88.0.1/32" {
		t.Fatalf("gateway received non-gateway address: %q", exchange.Device.WireGuardAddress)
	}
	gatewayReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateway/signed-config", nil)
	gatewayReq.Header.Set("Authorization", "Bearer "+exchange.EnrollmentToken)
	gatewayResp := httptest.NewRecorder()
	router.ServeHTTP(gatewayResp, gatewayReq)
	if gatewayResp.Code != http.StatusOK {
		t.Fatalf("gateway config status=%d body=%s", gatewayResp.Code, gatewayResp.Body.String())
	}
	var config GatewaySignedConfig
	if err := json.Unmarshal(gatewayResp.Body.Bytes(), &config); err != nil {
		t.Fatal(err)
	}
	if config.Role != RoleGateway || config.GatewayID != "gw-uat-1" || config.Generation != 2 || config.Signature.KeyID != "zero-key-1" {
		t.Fatalf("unexpected gateway config: %#v", config)
	}
	unsigned, err := gatewaySigningBytes(config)
	if err != nil || !ed25519.Verify(service.privateKey.Public().(ed25519.PublicKey), unsigned, mustDecodeBase64(t, config.Signature.Value)) {
		t.Fatalf("gateway signed config verification failed: err=%v", err)
	}
	if config.Transport.Kind != TransportVLESSXHTTP || config.Transport.Port != 443 || config.Transport.Path != DefaultTransportPath || config.Transport.Mode != DefaultTransportMode || config.Transport.Frontend != GatewayFrontendCaddyUnixH2C || config.Transport.ListenSocket != DefaultGatewayListenSocket {
		t.Fatalf("unexpected XHTTP Gateway transport contract: %#v", config.Transport)
	}
}

func mustDecodeBase64(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
