package overlay

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestOverlayPolicyCacheAndOmitPolicyJSON(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(fmt.Sprintf("file:overlay-cache-%d?mode=memory&cache=shared", time.Now().UnixNano())), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	_, signer, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	service, err := NewService(db, Config{SigningKeyID: "zero-key-1", SigningPrivateKey: signer, LocalProxyPort: 51831, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	owner := "11111111-2222-4333-8444-555555555555"
	gatewayKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))
	oneJoinToken, err := service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID: "net-cache-test", DisplayName: "Cache Test", CIDR: "10.88.0.0/24",
			GatewayID: "gw-cache-1", GatewayWireGuardKey: gatewayKey, GatewayWireGuardAddress: "10.88.0.1/32",
			GatewayEndpointHost: "gw.example.test", GatewayEndpointPort: 51820,
			TransportServerName: "gw.example.test", TransportPort: 443,
			TransportAuthID: "11111111-1111-1111-1111-111111111111", OwnerUserID: owner,
		},
		Invite: BootstrapInvite{Platform: "linux", Role: RoleOne, ExpiresAt: now.Add(time.Hour)},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	oneExchange, err := service.Exchange(t.Context(), ExchangeRequest{
		JoinToken:          oneJoinToken,
		DeviceID:           "one-cache-1",
		Name:               "One Cache 1",
		Hostname:           "one-cache-1.example.test",
		Platform:           "linux",
		Role:               RoleOne,
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}

	updated, err := service.AdminUpdatePolicy(t.Context(), owner, "net-cache-test", PolicyArtifact{
		DefaultAction: "deny",
		Rules: []PolicyRule{
			{
				ID:                 "allow-https",
				Action:             "allow",
				SourceDevices:      []string{"one-cache-1"},
				DestinationDevices: []string{"one-cache-2"},
				Protocols:          []string{"tcp"},
				Ports:              []int{443},
			},
		},
	})
	if err != nil {
		t.Fatalf("AdminUpdatePolicy gen+1 failed: %v", err)
	}

	// r.network must omit policy_json on hot path lookups.
	summary, err := service.repo.network(t.Context(), "net-cache-test")
	if err != nil {
		t.Fatalf("repo.network failed: %v", err)
	}
	if summary.PolicyJSON != "" {
		t.Fatalf("expected repo.network to omit policy_json, got %d bytes", len(summary.PolicyJSON))
	}
	if summary.ConfigGeneration != updated.Revision {
		t.Fatalf("generation=%d want %d", summary.ConfigGeneration, updated.Revision)
	}

	cfg, _, err := service.EnrollmentConfig(t.Context(), oneExchange.EnrollmentToken, "one-cache-1", "net-cache-test", true)
	if err != nil {
		t.Fatalf("EnrollmentConfig failed: %v", err)
	}
	if cfg.Policy == nil || cfg.Policy.Generation != updated.Revision {
		t.Fatalf("unexpected policy ref: %#v", cfg.Policy)
	}

	raw1, err := service.PolicyArtifact(t.Context(), oneExchange.EnrollmentToken, cfg.Policy.Generation, cfg.Policy.Digest)
	if err != nil {
		t.Fatalf("PolicyArtifact first call failed: %v", err)
	}
	sum := sha256.Sum256(raw1)
	if hex.EncodeToString(sum[:]) != cfg.Policy.Digest {
		t.Fatalf("digest mismatch: got %s want %s", hex.EncodeToString(sum[:]), cfg.Policy.Digest)
	}

	service.policyCacheMu.RLock()
	cachedEntry, cachedOK := service.policyCache[policyCacheKey{networkID: "net-cache-test", generation: updated.Revision}]
	service.policyCacheMu.RUnlock()
	if !cachedOK || len(cachedEntry.gatewayPolicy.Rules) != 1 || cachedEntry.gatewayPolicy.Rules[0].ID != "allow-https" {
		t.Fatalf("expected policy cache populated for gen %d, got ok=%v entry=%#v", updated.Revision, cachedOK, cachedEntry)
	}

	// Second call must hit cache and return identical bytes.
	raw2, err := service.PolicyArtifact(t.Context(), oneExchange.EnrollmentToken, cfg.Policy.Generation, cfg.Policy.Digest)
	if err != nil || !bytes.Equal(raw1, raw2) {
		t.Fatalf("PolicyArtifact second call mismatch: err=%v len1=%d len2=%d", err, len(raw1), len(raw2))
	}

	// Updating policy must evict older cached generation and serve the new revision.
	updated2, err := service.AdminUpdatePolicy(t.Context(), owner, "net-cache-test", PolicyArtifact{
		DefaultAction: "deny",
		Rules:         []PolicyRule{},
	})
	if err != nil {
		t.Fatalf("AdminUpdatePolicy gen+2 failed: %v", err)
	}
	cfg2, _, err := service.EnrollmentConfig(t.Context(), oneExchange.EnrollmentToken, "one-cache-1", "net-cache-test", true)
	if err != nil || cfg2.Policy == nil || cfg2.Policy.Generation != updated2.Revision {
		t.Fatalf("EnrollmentConfig after update failed: cfg=%#v err=%v", cfg2.Policy, err)
	}
	service.policyCacheMu.RLock()
	_, oldStillCached := service.policyCache[policyCacheKey{networkID: "net-cache-test", generation: updated.Revision}]
	newEntry, newCached := service.policyCache[policyCacheKey{networkID: "net-cache-test", generation: updated2.Revision}]
	service.policyCacheMu.RUnlock()
	if oldStillCached || !newCached || len(newEntry.gatewayPolicy.Rules) != 0 {
		t.Fatalf("cache eviction/replacement failed: old=%v new=%v entry=%#v", oldStillCached, newCached, newEntry)
	}
}

func TestOverlayHTTPPolicyAndConfigETagNotModified(t *testing.T) {
	_, router, joinToken := newOverlayHTTPTest(t)
	exBody, _ := json.Marshal(ExchangeRequest{
		JoinToken:          joinToken,
		DeviceID:           "one-etag-1",
		Name:               "One ETag",
		Hostname:           "one-etag.example.test",
		Platform:           "linux",
		Role:               RoleOne,
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{5}, 32)),
	})
	exReq := httptest.NewRequest(http.MethodPost, "/api/overlay/v1/join-tokens/exchange", bytes.NewReader(exBody))
	exReq.Header.Set("Content-Type", "application/json")
	exRec := httptest.NewRecorder()
	router.ServeHTTP(exRec, exReq)
	if exRec.Code != http.StatusOK {
		t.Fatalf("exchange status=%d body=%s", exRec.Code, exRec.Body.String())
	}
	var exResp ExchangeResponse
	if err := json.Unmarshal(exRec.Body.Bytes(), &exResp); err != nil {
		t.Fatal(err)
	}

	cfgReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config?device_id=one-etag-1", nil)
	cfgReq.Header.Set("Authorization", "Bearer "+exResp.EnrollmentToken)
	cfgReq.Header.Set("Accept", SignedConfigV2MediaType)
	cfgRec := httptest.NewRecorder()
	router.ServeHTTP(cfgRec, cfgReq)
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("signed-config status=%d body=%s", cfgRec.Code, cfgRec.Body.String())
	}
	if got := cfgRec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("signed-config Cache-Control=%q", got)
	}
	cfgETag := cfgRec.Header().Get("ETag")
	if cfgETag == "" {
		t.Fatal("signed-config missing ETag")
	}
	var cfg SignedConfig
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &cfg); err != nil || cfg.Policy == nil {
		t.Fatalf("decode signed-config err=%v policy=%#v", err, cfg.Policy)
	}

	cfg304Req := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config?device_id=one-etag-1", nil)
	cfg304Req.Header.Set("Authorization", "Bearer "+exResp.EnrollmentToken)
	cfg304Req.Header.Set("Accept", SignedConfigV2MediaType)
	cfg304Req.Header.Set("If-None-Match", cfgETag)
	cfg304Rec := httptest.NewRecorder()
	router.ServeHTTP(cfg304Rec, cfg304Req)
	if cfg304Rec.Code != http.StatusNotModified {
		t.Fatalf("signed-config If-None-Match status=%d want 304", cfg304Rec.Code)
	}
	if cfg304Rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("signed-config 304 Cache-Control=%q", cfg304Rec.Header().Get("Cache-Control"))
	}

	polReq := httptest.NewRequest(http.MethodGet, cfg.Policy.Path, nil)
	polReq.Header.Set("Authorization", "Bearer "+exResp.EnrollmentToken)
	polRec := httptest.NewRecorder()
	router.ServeHTTP(polRec, polReq)
	if polRec.Code != http.StatusOK {
		t.Fatalf("policy status=%d body=%s", polRec.Code, polRec.Body.String())
	}
	wantPolETag := `"` + cfg.Policy.Digest + `"`
	if got := polRec.Header().Get("ETag"); got != wantPolETag {
		t.Fatalf("policy ETag=%q want %q", got, wantPolETag)
	}
	if got := polRec.Header().Get("Cache-Control"); got != "private, no-store" {
		t.Fatalf("policy Cache-Control=%q", got)
	}

	pol304Req := httptest.NewRequest(http.MethodGet, cfg.Policy.Path, nil)
	pol304Req.Header.Set("Authorization", "Bearer "+exResp.EnrollmentToken)
	pol304Req.Header.Set("If-None-Match", wantPolETag)
	pol304Rec := httptest.NewRecorder()
	router.ServeHTTP(pol304Rec, pol304Req)
	if pol304Rec.Code != http.StatusNotModified {
		t.Fatalf("policy 304 status=%d want 304", pol304Rec.Code)
	}
	if pol304Rec.Header().Get("Cache-Control") != "private, no-store" || pol304Rec.Header().Get("ETag") != wantPolETag {
		t.Fatalf("policy 304 headers Cache-Control=%q ETag=%q", pol304Rec.Header().Get("Cache-Control"), pol304Rec.Header().Get("ETag"))
	}
}

func TestOverlayPolicyCacheConcurrentReads(t *testing.T) {
	service, _, joinToken := newOverlayHTTPTest(t)
	exResp, err := service.Exchange(t.Context(), ExchangeRequest{
		JoinToken:          joinToken,
		DeviceID:           "one-conc-1",
		Name:               "One Conc",
		Hostname:           "one-conc.example.test",
		Platform:           "linux",
		Role:               RoleOne,
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cfg, _, err := service.EnrollmentConfig(t.Context(), exResp.EnrollmentToken, "one-conc-1", "sit-private", true)
			if err != nil {
				errs <- err
				return
			}
			if _, err := service.PolicyArtifact(t.Context(), exResp.EnrollmentToken, cfg.Policy.Generation, cfg.Policy.Digest); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent read failed: %v", err)
	}
}
