package overlay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
)

func TestAdminExplainPolicyEvaluationAndValidation(t *testing.T) {
	service, _, _ := newOverlayHTTPTest(t)
	owner := "11111111-2222-4333-8444-555555555555"

	macRes, err := service.UserEnrollDevice(t.Context(), owner, UserEnrollRequest{
		NetworkID:          "sit-private",
		DeviceID:           "macbook",
		Platform:           "darwin",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{2}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	botRes, err := service.UserEnrollDevice(t.Context(), owner, UserEnrollRequest{
		NetworkID:          "sit-private",
		DeviceID:           "grok-bot",
		Platform:           "linux",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{3}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	dbRes, err := service.UserEnrollDevice(t.Context(), owner, UserEnrollRequest{
		NetworkID:          "sit-private",
		DeviceID:           "postgres-db",
		Platform:           "linux",
		WireGuardPublicKey: base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{4}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Default policy (no custom policy saved yet) fails closed via default_action.
	defaultResp, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "tcp",
		Port:                443,
	})
	if err != nil {
		t.Fatalf("explain default policy failed: %v", err)
	}
	if defaultResp.Decision != "deny" || defaultResp.DefaultAction != "deny" || defaultResp.Reason != "default_action" || defaultResp.MatchedRuleIndex != nil || defaultResp.MatchedRuleID != "" {
		t.Fatalf("unexpected default explain response: %#v", defaultResp)
	}
	if defaultResp.SourceAddress != macRes.Device.WireGuardAddress || defaultResp.DestinationAddress != botRes.Device.WireGuardAddress {
		t.Fatalf("unexpected resolved addresses: %#v", defaultResp)
	}

	updated, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{
		DefaultAction: "deny",
		Rules: []PolicyRule{
			{
				ID:                 "deny-ssh",
				Action:             "deny",
				SourceDevices:      []string{"macbook"},
				DestinationDevices: []string{"grok-bot"},
				Protocols:          []string{"tcp"},
				Ports:              []int{22},
			},
			{
				ID:                 "allow-mac-bot-web-icmp",
				Action:             "allow",
				SourceDevices:      []string{"macbook"},
				DestinationDevices: []string{"grok-bot"},
				Protocols:          []string{"icmp", "tcp"},
				Ports:              []int{443, 8080},
			},
			{
				ID:                 "allow-db-ports-any-proto",
				Action:             "allow",
				SourceDevices:      []string{"*"},
				DestinationDevices: []string{"postgres-db"},
				Protocols:          []string{},
				Ports:              []int{5432},
			},
			{
				ID:                 "allow-icmp-wild",
				Action:             "allow",
				SourceDevices:      []string{"*"},
				DestinationDevices: []string{"grok-bot"},
				Protocols:          []string{"icmp"},
			},
		},
	})
	if err != nil {
		t.Fatalf("AdminUpdatePolicy failed: %v", err)
	}

	// Index 0 match (first-match-wins) must include matched_rule_index: 0 in JSON.
	sshResp, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "TCP",
		Port:                22,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sshResp.Decision != "deny" || sshResp.MatchedRuleID != "deny-ssh" || sshResp.MatchedRuleIndex == nil || *sshResp.MatchedRuleIndex != 0 || sshResp.Reason != "matched_rule" || sshResp.Revision != updated.Revision {
		t.Fatalf("unexpected ssh explain response: %#v", sshResp)
	}
	rawSSH, _ := json.Marshal(sshResp)
	if !bytes.Contains(rawSSH, []byte(`"matched_rule_index":0`)) {
		t.Fatalf("expected matched_rule_index:0 in JSON, got %s", rawSSH)
	}

	// Rule 1 matches both tcp:8080 and icmp.
	webResp, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "tcp",
		Port:                8080,
	})
	if err != nil || webResp.Decision != "allow" || webResp.MatchedRuleID != "allow-mac-bot-web-icmp" || webResp.MatchedRuleIndex == nil || *webResp.MatchedRuleIndex != 1 {
		t.Fatalf("unexpected web explain response: %#v err=%v", webResp, err)
	}
	icmpResp, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "icmp",
	})
	if err != nil || icmpResp.Decision != "allow" || icmpResp.MatchedRuleID != "allow-mac-bot-web-icmp" || icmpResp.MatchedRuleIndex == nil || *icmpResp.MatchedRuleIndex != 1 {
		t.Fatalf("unexpected icmp explain response: %#v err=%v", icmpResp, err)
	}

	// Empty protocols + ports resolves to tcp/udp only (matches udp:5432, not icmp).
	dbUDP, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "offline-laptop",
		DestinationDeviceID: "postgres-db",
		Protocol:            "udp",
		Port:                5432,
	})
	if err != nil || dbUDP.Decision != "allow" || dbUDP.MatchedRuleID != "allow-db-ports-any-proto" || dbUDP.SourceAddress != "" || dbUDP.DestinationAddress != dbRes.Device.WireGuardAddress {
		t.Fatalf("unexpected empty-proto-with-port explain response: %#v err=%v", dbUDP, err)
	}
	dbICMP, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "postgres-db",
		Protocol:            "icmp",
	})
	if err != nil || dbICMP.Decision != "deny" || dbICMP.Reason != "default_action" || dbICMP.MatchedRuleIndex != nil {
		t.Fatalf("expected icmp to postgres-db to fall back to deny, got %#v err=%v", dbICMP, err)
	}

	// Wildcard source matches even when source device is not enrolled yet.
	wildICMP, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "future-device",
		DestinationDeviceID: "grok-bot",
		Protocol:            "icmp",
	})
	if err != nil || wildICMP.Decision != "allow" || wildICMP.MatchedRuleID != "allow-icmp-wild" {
		t.Fatalf("unexpected wildcard explain response: %#v err=%v", wildICMP, err)
	}

	// DefaultAction="allow" fallback.
	if _, err := service.AdminUpdatePolicy(t.Context(), owner, "sit-private", PolicyArtifact{
		DefaultAction: "allow",
		Rules:         []PolicyRule{},
	}); err != nil {
		t.Fatal(err)
	}
	allowFallback, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "all",
	})
	if err != nil || allowFallback.Decision != "allow" || allowFallback.DefaultAction != "allow" || allowFallback.Reason != "default_action" {
		t.Fatalf("unexpected allow fallback response: %#v err=%v", allowFallback, err)
	}

	// Owner isolation.
	if _, err := service.AdminExplainPolicy(t.Context(), "other-owner", "sit-private", PolicyExplainRequest{
		SourceDeviceID:      "macbook",
		DestinationDeviceID: "grok-bot",
		Protocol:            "tcp",
		Port:                443,
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expected ErrNotFound for non-owner explain, got %v", err)
	}

	// Validation errors.
	badRequests := []PolicyExplainRequest{
		{SourceDeviceID: "", DestinationDeviceID: "grok-bot", Protocol: "tcp", Port: 443},
		{SourceDeviceID: "macbook", DestinationDeviceID: "", Protocol: "tcp", Port: 443},
		{SourceDeviceID: "*", DestinationDeviceID: "grok-bot", Protocol: "tcp", Port: 443},
		{SourceDeviceID: "10.77.0.2", DestinationDeviceID: "grok-bot", Protocol: "tcp", Port: 443},
		{SourceDeviceID: "macbook", DestinationDeviceID: "grok-bot", Protocol: "gre"},
		{SourceDeviceID: "macbook", DestinationDeviceID: "grok-bot", Protocol: ""},
		{SourceDeviceID: "macbook", DestinationDeviceID: "grok-bot", Protocol: "icmp", Port: 80},
		{SourceDeviceID: "macbook", DestinationDeviceID: "grok-bot", Protocol: "all", Port: 443},
		{SourceDeviceID: "macbook", DestinationDeviceID: "grok-bot", Protocol: "tcp", Port: -1},
		{SourceDeviceID: "macbook", DestinationDeviceID: "grok-bot", Protocol: "tcp", Port: 65536},
	}
	for _, req := range badRequests {
		if _, err := service.AdminExplainPolicy(t.Context(), owner, "sit-private", req); !errors.Is(err, ErrInvalidInput) {
			t.Fatalf("expected ErrInvalidInput for %#v, got %v", req, err)
		}
	}
}
