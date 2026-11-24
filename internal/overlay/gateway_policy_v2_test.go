package overlay

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// xconnectGatewayV2SigningBytes mirrors XConnect-Gateway v0.1.14
// internal/gateway/contract.go Config.signingBytes() verbatim to enforce
// cross-repo Ed25519 signature parity.
func xconnectGatewayV2SigningBytes(t *testing.T, raw []byte) (int, []byte) {
	t.Helper()
	var cfg struct {
		SchemaVersion  int              `json:"schema_version"`
		Role           string           `json:"role"`
		ConfigID       string           `json:"config_id"`
		NetworkID      string           `json:"network_id"`
		GatewayID      string           `json:"gateway_id"`
		Generation     uint64           `json:"generation"`
		IssuedAt       time.Time        `json:"issued_at"`
		ExpiresAt      time.Time        `json:"expires_at"`
		ConnectionMode string           `json:"connection_mode,omitempty"`
		InterfaceName  string           `json:"interface_name"`
		Address        string           `json:"address"`
		ListenPort     int              `json:"listen_port"`
		MTU            int              `json:"mtu"`
		Peers          []GatewayPeer    `json:"peers"`
		Transport      GatewayTransport `json:"transport"`
		Mesh           *RelaySpec       `json:"mesh,omitempty"`
		Policy         *PolicyArtifact  `json:"policy,omitempty"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("unmarshal gateway config for parity check: %v", err)
	}
	if cfg.SchemaVersion == 1 {
		b, err := json.Marshal(struct {
			SchemaVersion  int              `json:"schema_version"`
			Role           string           `json:"role"`
			ConfigID       string           `json:"config_id"`
			NetworkID      string           `json:"network_id"`
			GatewayID      string           `json:"gateway_id"`
			Generation     uint64           `json:"generation"`
			IssuedAt       time.Time        `json:"issued_at"`
			ExpiresAt      time.Time        `json:"expires_at"`
			ConnectionMode string           `json:"connection_mode,omitempty"`
			InterfaceName  string           `json:"interface_name"`
			Address        string           `json:"address"`
			ListenPort     int              `json:"listen_port"`
			MTU            int              `json:"mtu"`
			Peers          []GatewayPeer    `json:"peers"`
			Transport      GatewayTransport `json:"transport"`
			Mesh           *RelaySpec       `json:"mesh,omitempty"`
		}{cfg.SchemaVersion, cfg.Role, cfg.ConfigID, cfg.NetworkID, cfg.GatewayID, cfg.Generation, cfg.IssuedAt, cfg.ExpiresAt, strings.TrimSpace(cfg.ConnectionMode), cfg.InterfaceName, cfg.Address, cfg.ListenPort, cfg.MTU, cfg.Peers, cfg.Transport, cfg.Mesh})
		if err != nil {
			t.Fatal(err)
		}
		return cfg.SchemaVersion, b
	}
	b, err := json.Marshal(struct {
		SchemaVersion  int              `json:"schema_version"`
		Role           string           `json:"role"`
		ConfigID       string           `json:"config_id"`
		NetworkID      string           `json:"network_id"`
		GatewayID      string           `json:"gateway_id"`
		Generation     uint64           `json:"generation"`
		IssuedAt       time.Time        `json:"issued_at"`
		ExpiresAt      time.Time        `json:"expires_at"`
		ConnectionMode string           `json:"connection_mode,omitempty"`
		InterfaceName  string           `json:"interface_name"`
		Address        string           `json:"address"`
		ListenPort     int              `json:"listen_port"`
		MTU            int              `json:"mtu"`
		Peers          []GatewayPeer    `json:"peers"`
		Transport      GatewayTransport `json:"transport"`
		Mesh           *RelaySpec       `json:"mesh,omitempty"`
		Policy         *PolicyArtifact  `json:"policy,omitempty"`
	}{cfg.SchemaVersion, cfg.Role, cfg.ConfigID, cfg.NetworkID, cfg.GatewayID, cfg.Generation, cfg.IssuedAt, cfg.ExpiresAt, strings.TrimSpace(cfg.ConnectionMode), cfg.InterfaceName, cfg.Address, cfg.ListenPort, cfg.MTU, cfg.Peers, cfg.Transport, cfg.Mesh, cfg.Policy})
	if err != nil {
		t.Fatal(err)
	}
	return cfg.SchemaVersion, b
}

func TestGatewaySignedConfigV1BackwardCompatibilityAndV2Policy(t *testing.T) {
	service, router, _ := newOverlayHTTPTest(t)
	owner := "11111111-2222-4333-8444-555555555555"
	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

	gwJoin, err := service.AdminCreateInvite(t.Context(), owner, "https://accounts.example.test", AdminInviteRequest{
		NetworkID: "sit-private",
		DeviceID:  "gw-sit-1",
		Platform:  "linux",
		Role:      RoleGateway,
		ExpiresAt: service.now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	gwToken := strings.TrimPrefix(strings.Split(gwJoin.JoinURI, "?")[0], "xconnect://join/")
	gwExchange, err := service.Exchange(t.Context(), ExchangeRequest{
		JoinToken:          gwToken,
		DeviceID:           "gw-sit-1",
		Platform:           "linux",
		Role:               RoleGateway,
		WireGuardPublicKey: gwKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.UserEnrollDevice(t.Context(), owner, UserEnrollRequest{
		NetworkID:          "sit-private",
		DeviceID:           "macbook",
		Platform:           "darwin",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.UserEnrollDevice(t.Context(), owner, UserEnrollRequest{
		NetworkID:          "sit-private",
		DeviceID:           "grok-bot",
		Platform:           "linux",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{10}, 32)),
	}); err != nil {
		t.Fatal(err)
	}

	updatedPolicy, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{
		DefaultAction: "deny",
		Rules: []PolicyRule{
			{
				ID:                 "allow-mac-grok-web",
				Action:             "allow",
				SourceDevices:      []string{"macbook"},
				DestinationDevices: []string{"grok-bot"},
				Protocols:          []string{"ICMP", "TCP"},
				Ports:              []int{8080, 22, 8080},
			},
		},
	})
	if err != nil {
		t.Fatalf("AdminUpdatePolicy error: %v", err)
	}
	if len(updatedPolicy.ProtectedFlows) == 0 {
		t.Fatal("expected mandatory default protected_flows to be preserved")
	}

	// 1. Legacy Accept: application/json must return SchemaVersion: 1 without Policy.
	v1Req := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateway/signed-config", nil)
	v1Req.Header.Set("Authorization", "Bearer "+gwExchange.EnrollmentToken)
	v1Req.Header.Set("Accept", "application/json")
	v1Resp := httptest.NewRecorder()
	router.ServeHTTP(v1Resp, v1Req)
	if v1Resp.Code != http.StatusOK || v1Resp.Header().Get("Content-Type") != "application/json" || v1Resp.Header().Get("Vary") != "Accept" {
		t.Fatalf("unexpected v1 response status=%d headers=%v body=%s", v1Resp.Code, v1Resp.Header(), v1Resp.Body.String())
	}
	if bytes.Contains(v1Resp.Body.Bytes(), []byte(`"policy"`)) {
		t.Fatalf("legacy v1 gateway config unexpectedly contained policy: %s", v1Resp.Body.String())
	}
	var v1Cfg GatewaySignedConfig
	if err := json.Unmarshal(v1Resp.Body.Bytes(), &v1Cfg); err != nil {
		t.Fatal(err)
	}
	if v1Cfg.SchemaVersion != 1 || v1Cfg.Policy != nil || v1Cfg.ConnectionMode != DefaultConnectionMode {
		t.Fatalf("unexpected v1 config: %#v", v1Cfg)
	}
	pubKey := service.privateKey.Public().(ed25519.PublicKey)
	sv1, v1Bytes := xconnectGatewayV2SigningBytes(t, v1Resp.Body.Bytes())
	if sv1 != 1 || !ed25519.Verify(pubKey, v1Bytes, mustDecodeBase64(t, v1Cfg.Signature.Value)) {
		t.Fatalf("v1 gateway signature did not match XConnect-Gateway signing payload")
	}

	// 2. V2 Accept header (including comma-separated fallback) returns SchemaVersion: 2 + Policy.
	v2Req := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/gateway/signed-config", nil)
	v2Req.Header.Set("Authorization", "Bearer "+gwExchange.EnrollmentToken)
	v2Req.Header.Set("Accept", GatewaySignedConfigV2MediaType+", application/json")
	v2Resp := httptest.NewRecorder()
	router.ServeHTTP(v2Resp, v2Req)
	if v2Resp.Code != http.StatusOK || v2Resp.Header().Get("Content-Type") != GatewaySignedConfigV2MediaType || v2Resp.Header().Get("Vary") != "Accept" {
		t.Fatalf("unexpected v2 response status=%d headers=%v body=%s", v2Resp.Code, v2Resp.Header(), v2Resp.Body.String())
	}
	var v2Cfg GatewaySignedConfig
	if err := json.Unmarshal(v2Resp.Body.Bytes(), &v2Cfg); err != nil {
		t.Fatal(err)
	}
	if v2Cfg.SchemaVersion != 2 || v2Cfg.Policy == nil || v2Cfg.Policy.Revision != v2Cfg.Generation || len(v2Cfg.Policy.Rules) != 1 {
		t.Fatalf("unexpected v2 config: %#v", v2Cfg)
	}
	if len(v2Cfg.Policy.Rules[0].Ports) != 2 || v2Cfg.Policy.Rules[0].Ports[0] != 22 || v2Cfg.Policy.Rules[0].Ports[1] != 8080 {
		t.Fatalf("expected deduplicated sorted ports [22 8080], got %#v", v2Cfg.Policy.Rules[0].Ports)
	}
	sv2, v2Bytes := xconnectGatewayV2SigningBytes(t, v2Resp.Body.Bytes())
	serviceV2Bytes, err := gatewaySigningBytes(v2Cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(v2Bytes, serviceV2Bytes) {
		t.Fatalf("signing payload mismatch:\nXConnect-Gateway: %s\nAccounts:         %s", v2Bytes, serviceV2Bytes)
	}
	if sv2 != 2 || !ed25519.Verify(pubKey, v2Bytes, mustDecodeBase64(t, v2Cfg.Signature.Value)) {
		t.Fatalf("v2 gateway signature failed XConnect-Gateway verification")
	}
}

func TestPolicyGenerationSyncAcrossDeviceRegistrationAndFrontendReseed(t *testing.T) {
	service, _, _ := newOverlayHTTPTest(t)
	owner := "11111111-2222-4333-8444-555555555555"
	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

	gwJoin, err := service.AdminCreateInvite(t.Context(), owner, "https://accounts.example.test", AdminInviteRequest{
		NetworkID: "sit-private",
		DeviceID:  "gw-sit-1",
		Platform:  "linux",
		Role:      RoleGateway,
		ExpiresAt: service.now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	gwToken := strings.TrimPrefix(strings.Split(gwJoin.JoinURI, "?")[0], "xconnect://join/")
	gwExchange, err := service.Exchange(t.Context(), ExchangeRequest{
		JoinToken:          gwToken,
		DeviceID:           "gw-sit-1",
		Platform:           "linux",
		Role:               RoleGateway,
		WireGuardPublicKey: gwKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{
		DefaultAction: "deny",
		Rules: []PolicyRule{
			{
				ID:                 "allow-all-web",
				Action:             "allow",
				SourceDevices:      []string{"*"},
				DestinationDevices: []string{"*"},
				Protocols:          []string{"tcp"},
				Ports:              []int{443},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	reg, err := service.Register(t.Context(), RegistrationRequest{
		NetworkID:          "sit-private",
		DeviceID:           "one-self-reg",
		Name:               "Self Registered",
		Platform:           "linux",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{19}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.AdminApproveRegistration(t.Context(), owner, reg.RegistrationID, "sit-private"); err != nil {
		t.Fatal(err)
	}
	exchange, _, err := service.ExchangeRegistration(t.Context(), reg.RegistrationID, reg.RegistrationToken)
	if err != nil {
		t.Fatalf("ExchangeRegistration failed: %v", err)
	}

	var netRecord NetworkRecord
	if err := service.repo.DB.Where("id = ?", "sit-private").First(&netRecord).Error; err != nil {
		t.Fatal(err)
	}
	policy, err := service.AdminPolicy(t.Context(), owner, "sit-private")
	if err != nil {
		t.Fatalf("AdminPolicy after ExchangeRegistration failed: %v", err)
	}
	if policy.Revision != netRecord.ConfigGeneration || exchange.Network.ConfigGeneration != netRecord.ConfigGeneration {
		t.Fatalf("generation/revision mismatch after ExchangeRegistration: net=%d exchange=%d policy=%d", netRecord.ConfigGeneration, exchange.Network.ConfigGeneration, policy.Revision)
	}

	gwCfg, _, err := service.GatewayConfig(t.Context(), gwExchange.EnrollmentToken, true)
	if err != nil {
		t.Fatalf("GatewayConfig(v2) after ExchangeRegistration failed: %v", err)
	}
	if gwCfg.Policy == nil || gwCfg.Policy.Revision != gwCfg.Generation {
		t.Fatalf("gateway policy revision out of sync: %#v", gwCfg.Policy)
	}

	// Reseeding with a new frontend also advances generation and keeps policy revision synced.
	if _, err := service.Seed(t.Context(), BootstrapConfig{
		Network: BootstrapNetwork{
			ID:                      "sit-private",
			DisplayName:             "SIT private",
			CIDR:                    "10.77.0.0/29",
			GatewayID:               "gw-sit-1",
			GatewayWireGuardKey:     gwKey,
			GatewayWireGuardAddress: "10.77.0.1/32",
			GatewayEndpointHost:     "gw.example.test",
			GatewayEndpointPort:     51820,
			TransportServerName:     "gw.example.test",
			TransportPort:           443,
			TransportAuthID:         "11111111-1111-1111-1111-111111111111",
			GatewayFrontend:         GatewayFrontendCaddyUnixH2C,
			GatewayListenSocket:     DefaultGatewayListenSocket,
			OwnerUserID:             owner,
		},
		Invite: BootstrapInvite{Platform: "linux", Role: RoleOne, ExpiresAt: service.now().Add(time.Hour)},
	}, ""); err != nil {
		t.Fatal(err)
	}
	gwCfgAfterReseed, _, err := service.GatewayConfig(t.Context(), gwExchange.EnrollmentToken, true)
	if err != nil {
		t.Fatalf("GatewayConfig(v2) after frontend reseed failed: %v", err)
	}
	if gwCfgAfterReseed.Generation != gwCfg.Generation+1 || gwCfgAfterReseed.Policy == nil || gwCfgAfterReseed.Policy.Revision != gwCfgAfterReseed.Generation {
		t.Fatalf("expected generation and policy revision bump after reseed, got gen=%d policy=%#v", gwCfgAfterReseed.Generation, gwCfgAfterReseed.Policy)
	}
}

func TestAdminUpdatePolicyValidation(t *testing.T) {
	service, _, _ := newOverlayHTTPTest(t)
	owner := "11111111-2222-4333-8444-555555555555"

	tooManyRules := make([]PolicyRule, 201)
	for i := range tooManyRules {
		tooManyRules[i] = PolicyRule{
			ID:                 "rule-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i/26)),
			Action:             "allow",
			SourceDevices:      []string{"*"},
			DestinationDevices: []string{"*"},
		}
	}

	invalidCases := []struct {
		name   string
		policy PolicyArtifact
	}{
		{name: "bad schema version", policy: PolicyArtifact{SchemaVersion: 2, DefaultAction: "deny"}},
		{name: "network mismatch", policy: PolicyArtifact{NetworkID: "other-net", DefaultAction: "deny"}},
		{name: "bad default action", policy: PolicyArtifact{DefaultAction: "reject"}},
		{name: "too many rules", policy: PolicyArtifact{DefaultAction: "deny", Rules: tooManyRules}},
		{name: "empty rule id", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "   ", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}}}}},
		{name: "bad rule id chars", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "bad rule", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}}}}},
		{name: "too long rule id", policy: PolicyArtifact{Rules: []PolicyRule{{ID: strings.Repeat("a", 65), Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}}}}},
		{name: "duplicate rule id", policy: PolicyArtifact{Rules: []PolicyRule{
			{ID: "dup", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}},
			{ID: "dup", Action: "deny", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}},
		}}},
		{name: "bad rule action accept", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "accept", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}}}}},
		{name: "empty source devices", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{}, DestinationDevices: []string{"*"}}}}},
		{name: "empty dest devices", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: nil}}}},
		{name: "hardcoded IP source device", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"10.66.0.2"}, DestinationDevices: []string{"*"}}}}},
		{name: "cidr source device", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"10.66.0.0/24"}, DestinationDevices: []string{"*"}}}}},
		{name: "blank device ref", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{" "}, DestinationDevices: []string{"*"}}}}},
		{name: "bad protocol", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}, Protocols: []string{"gre"}}}}},
		{name: "empty protocol item", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}, Protocols: []string{""}}}}},
		{name: "icmp only with ports", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}, Protocols: []string{"icmp"}, Ports: []int{80}}}}},
		{name: "all with ports", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}, Protocols: []string{"all"}, Ports: []int{443}}}}},
		{name: "port zero", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}, Protocols: []string{"tcp"}, Ports: []int{0}}}}},
		{name: "port out of range", policy: PolicyArtifact{Rules: []PolicyRule{{ID: "r1", Action: "allow", SourceDevices: []string{"*"}, DestinationDevices: []string{"*"}, Protocols: []string{"tcp"}, Ports: []int{65536}}}}},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", tc.policy); !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("%s: expected ErrInvalidInput, got %v", tc.name, err)
			}
		})
	}

	// Empty policy normalizes default_action="deny", rules=[], protected_flows populated.
	saved, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{})
	if err != nil {
		t.Fatalf("empty PolicyArtifact update failed: %v", err)
	}
	if saved.DefaultAction != "deny" || saved.Rules == nil || len(saved.Rules) != 0 || len(saved.ProtectedFlows) == 0 {
		t.Fatalf("unexpected normalized empty policy: %#v", saved)
	}
	raw, err := json.Marshal(saved)
	if err != nil || !bytes.Contains(raw, []byte(`"rules":[]`)) {
		t.Fatalf("expected non-null empty rules array in JSON: %s err=%v", raw, err)
	}
}

func TestClientPolicyArtifactCompatibilityWithDefaultAllow(t *testing.T) {
	service, router, _ := newOverlayHTTPTest(t)
	owner := "11111111-2222-4333-8444-555555555555"
	gwKey := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))

	gwJoin, err := service.AdminCreateInvite(t.Context(), owner, "https://accounts.example.test", AdminInviteRequest{
		NetworkID: "sit-private",
		DeviceID:  "gw-sit-1",
		Platform:  "linux",
		Role:      RoleGateway,
		ExpiresAt: service.now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	gwToken := strings.TrimPrefix(strings.Split(gwJoin.JoinURI, "?")[0], "xconnect://join/")
	gwExchange, err := service.Exchange(t.Context(), ExchangeRequest{
		JoinToken:          gwToken,
		DeviceID:           "gw-sit-1",
		Platform:           "linux",
		Role:               RoleGateway,
		WireGuardPublicKey: gwKey,
	})
	if err != nil {
		t.Fatal(err)
	}

	oneEnroll, err := service.UserEnrollDevice(t.Context(), owner, UserEnrollRequest{
		NetworkID:          "sit-private",
		DeviceID:           "one-client-compat",
		Platform:           "darwin",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{11}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}

	if _, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{
		DefaultAction: "allow",
		Rules: []PolicyRule{
			{
				ID:                 "allow-all-peers",
				Action:             "allow",
				SourceDevices:      []string{"*"},
				DestinationDevices: []string{"*"},
				Protocols:          []string{"all"},
			},
		},
	}); err != nil {
		t.Fatalf("AdminUpdatePolicy(default=allow) error: %v", err)
	}

	// Gateway receives full V2 policy with default_action="allow" and wildcard rule.
	gwCfg, _, err := service.GatewayConfig(t.Context(), gwExchange.EnrollmentToken, true)
	if err != nil {
		t.Fatalf("GatewayConfig(v2) error: %v", err)
	}
	if gwCfg.Policy == nil || gwCfg.Policy.DefaultAction != "allow" || len(gwCfg.Policy.Rules) != 1 {
		t.Fatalf("expected gateway policy default_action=allow with 1 rule, got %#v", gwCfg.Policy)
	}

	// XConnect-One client receives canonical default_action="deny" artifact whose digest matches SignedConfig.Policy.
	oneReq := httptest.NewRequest(http.MethodGet, "/api/overlay/v1/enrollment/signed-config?device_id=one-client-compat&network_id=sit-private", nil)
	oneReq.Header.Set("Authorization", "Bearer "+oneEnroll.EnrollmentToken)
	oneReq.Header.Set("Accept", SignedConfigV2MediaType)
	oneResp := httptest.NewRecorder()
	router.ServeHTTP(oneResp, oneReq)
	if oneResp.Code != http.StatusOK {
		t.Fatalf("enrollment signed-config v2 status=%d body=%s", oneResp.Code, oneResp.Body.String())
	}
	var oneCfg SignedConfig
	if err := json.Unmarshal(oneResp.Body.Bytes(), &oneCfg); err != nil || oneCfg.Policy == nil {
		t.Fatalf("decode enrollment signed-config v2 err=%v cfg=%#v", err, oneCfg)
	}

	artReq := httptest.NewRequest(http.MethodGet, oneCfg.Policy.Path, nil)
	artReq.Header.Set("Authorization", "Bearer "+oneEnroll.EnrollmentToken)
	artResp := httptest.NewRecorder()
	router.ServeHTTP(artResp, artReq)
	if artResp.Code != http.StatusOK || artResp.Header().Get("Content-Type") != PolicyMediaType {
		t.Fatalf("policy artifact status=%d ct=%q body=%s", artResp.Code, artResp.Header().Get("Content-Type"), artResp.Body.String())
	}
	var clientPolicy PolicyArtifact
	if err := json.Unmarshal(artResp.Body.Bytes(), &clientPolicy); err != nil {
		t.Fatalf("decode client policy artifact: %v", err)
	}
	if clientPolicy.DefaultAction != "deny" || len(clientPolicy.Rules) != 0 || clientPolicy.Revision != oneCfg.Generation {
		t.Fatalf("expected canonical client policy artifact (default=deny, rules=[], rev=%d), got %#v", oneCfg.Generation, clientPolicy)
	}
}
