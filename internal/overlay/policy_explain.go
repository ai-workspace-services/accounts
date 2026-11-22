package overlay

import (
	"context"
	"errors"
	"net/netip"
	"slices"
	"strings"

	"gorm.io/gorm"
)

func validateAndNormalizePolicy(networkID string, generation uint64, policy PolicyArtifact) (PolicyArtifact, error) {
	if policy.SchemaVersion != 0 && policy.SchemaVersion != 1 {
		return PolicyArtifact{}, ErrInvalidInput
	}
	if trimmedNet := strings.TrimSpace(policy.NetworkID); trimmedNet != "" && trimmedNet != networkID {
		return PolicyArtifact{}, ErrInvalidInput
	}
	defaultAction := strings.ToLower(strings.TrimSpace(policy.DefaultAction))
	if defaultAction == "" {
		defaultAction = "deny"
	}
	if defaultAction != "deny" && defaultAction != "allow" {
		return PolicyArtifact{}, ErrInvalidInput
	}
	if len(policy.Rules) > maxPolicyRules {
		return PolicyArtifact{}, ErrInvalidInput
	}

	protectedFlows := make([]string, 0, len(policy.ProtectedFlows))
	seenFlows := make(map[string]struct{}, len(policy.ProtectedFlows))
	for _, rawFlow := range policy.ProtectedFlows {
		flow := strings.TrimSpace(rawFlow)
		if flow == "" {
			continue
		}
		if _, exists := seenFlows[flow]; exists {
			continue
		}
		seenFlows[flow] = struct{}{}
		protectedFlows = append(protectedFlows, flow)
	}
	if len(protectedFlows) == 0 {
		protectedFlows = defaultPolicyArtifact(networkID, generation).ProtectedFlows
	}

	seenRuleIDs := make(map[string]struct{}, len(policy.Rules))
	normalizedRules := make([]PolicyRule, 0, len(policy.Rules))
	for _, rule := range policy.Rules {
		ruleID := strings.TrimSpace(rule.ID)
		if !policyRuleIDPattern.MatchString(ruleID) {
			return PolicyArtifact{}, ErrInvalidInput
		}
		if _, dup := seenRuleIDs[ruleID]; dup {
			return PolicyArtifact{}, ErrInvalidInput
		}
		seenRuleIDs[ruleID] = struct{}{}

		action := strings.ToLower(strings.TrimSpace(rule.Action))
		if action != "allow" && action != "deny" {
			return PolicyArtifact{}, ErrInvalidInput
		}

		srcDevices, ok := normalizePolicyDeviceRefs(rule.SourceDevices)
		if !ok {
			return PolicyArtifact{}, ErrInvalidInput
		}
		dstDevices, ok := normalizePolicyDeviceRefs(rule.DestinationDevices)
		if !ok {
			return PolicyArtifact{}, ErrInvalidInput
		}

		ports, err := normalizePolicyPorts(rule.Ports)
		if err != nil {
			return PolicyArtifact{}, err
		}
		protocols, err := normalizePolicyProtocolsForSave(rule.Protocols, len(ports) > 0)
		if err != nil {
			return PolicyArtifact{}, err
		}

		normalizedRules = append(normalizedRules, PolicyRule{
			WholeDevice:        rule.WholeDevice,
			ID:                 ruleID,
			Action:             action,
			SourceDevices:      srcDevices,
			DestinationDevices: dstDevices,
			Protocols:          protocols,
			Ports:              ports,
		})
	}

	return PolicyArtifact{
		SchemaVersion:   1,
		CompilerVersion: PolicyCompilerVersion,
		NetworkID:       networkID,
		Revision:        generation,
		DefaultAction:   defaultAction,
		ProtectedFlows:  protectedFlows,
		Rules:           normalizedRules,
	}, nil
}

func validPolicyDeviceID(id string) bool {
	if !overlayDeviceIDPattern.MatchString(id) {
		return false
	}
	if _, err := netip.ParseAddr(id); err == nil {
		return false
	}
	return true
}

func normalizePolicyDeviceRefs(raw []string) ([]string, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	seen := make(map[string]struct{}, len(raw))
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		ref := strings.TrimSpace(entry)
		if ref == "" {
			return nil, false
		}
		if ref != "*" && !validPolicyDeviceID(ref) {
			return nil, false
		}
		if _, exists := seen[ref]; exists {
			continue
		}
		seen[ref] = struct{}{}
		out = append(out, ref)
	}
	if len(out) == 0 {
		return nil, false
	}
	return out, true
}

func normalizePolicyPorts(raw []int) ([]int, error) {
	if len(raw) == 0 {
		return []int{}, nil
	}
	seen := make(map[int]struct{}, len(raw))
	ports := make([]int, 0, len(raw))
	for _, port := range raw {
		if port < 1 || port > 65535 {
			return nil, ErrInvalidInput
		}
		if _, exists := seen[port]; exists {
			continue
		}
		seen[port] = struct{}{}
		ports = append(ports, port)
	}
	slices.Sort(ports)
	return ports, nil
}

func normalizePolicyProtocolsForSave(raw []string, hasPorts bool) ([]string, error) {
	if len(raw) == 0 {
		return []string{}, nil
	}
	seen := make(map[string]struct{}, len(raw))
	ordered := make([]string, 0, len(raw))
	hasAll := false
	hasNonICMP := false
	for _, entry := range raw {
		proto := strings.ToLower(strings.TrimSpace(entry))
		switch proto {
		case "all":
			hasAll = true
			hasNonICMP = true
		case "tcp", "udp":
			hasNonICMP = true
		case "icmp":
		default:
			return nil, ErrInvalidInput
		}
		if _, exists := seen[proto]; exists {
			continue
		}
		seen[proto] = struct{}{}
		ordered = append(ordered, proto)
	}
	if hasAll && hasPorts {
		return nil, ErrInvalidInput
	}
	if !hasNonICMP && hasPorts {
		return nil, ErrInvalidInput
	}
	if hasAll {
		return []string{"all"}, nil
	}
	return ordered, nil
}

func effectiveRuleProtocols(raw []string, hasPorts bool) ([]string, error) {
	if len(raw) == 0 {
		if hasPorts {
			return []string{"tcp", "udp"}, nil
		}
		return []string{"all"}, nil
	}
	seen := make(map[string]struct{}, len(raw))
	ordered := make([]string, 0, len(raw))
	hasAll := false
	hasNonICMP := false
	for _, entry := range raw {
		proto := strings.ToLower(strings.TrimSpace(entry))
		switch proto {
		case "all":
			hasAll = true
			hasNonICMP = true
		case "tcp", "udp":
			hasNonICMP = true
		case "icmp":
		default:
			return nil, ErrInvalidInput
		}
		if _, exists := seen[proto]; exists {
			continue
		}
		seen[proto] = struct{}{}
		ordered = append(ordered, proto)
	}
	if hasPorts && !hasNonICMP {
		return nil, ErrInvalidInput
	}
	if hasAll {
		if hasPorts {
			return []string{"tcp", "udp"}, nil
		}
		return []string{"all"}, nil
	}
	return ordered, nil
}

func (s *Service) AdminExplainPolicy(ctx context.Context, ownerUserID, networkID string, req PolicyExplainRequest) (PolicyExplainResponse, error) {
	ownerUserID = strings.TrimSpace(ownerUserID)
	networkID = strings.TrimSpace(networkID)
	if ownerUserID == "" || networkID == "" {
		return PolicyExplainResponse{}, ErrNotFound
	}

	var network NetworkRecord
	if err := s.repo.DB.WithContext(ctx).Where("id = ? AND owner_user_id = ?", networkID, ownerUserID).First(&network).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return PolicyExplainResponse{}, ErrNotFound
		}
		return PolicyExplainResponse{}, err
	}

	req.SourceDeviceID = strings.TrimSpace(req.SourceDeviceID)
	req.DestinationDeviceID = strings.TrimSpace(req.DestinationDeviceID)
	req.Protocol = strings.ToLower(strings.TrimSpace(req.Protocol))
	if !validPolicyDeviceID(req.SourceDeviceID) || !validPolicyDeviceID(req.DestinationDeviceID) {
		return PolicyExplainResponse{}, ErrInvalidInput
	}
	switch req.Protocol {
	case "tcp", "udp", "icmp", "all":
	default:
		return PolicyExplainResponse{}, ErrInvalidInput
	}
	if req.Port < 0 || req.Port > 65535 {
		return PolicyExplainResponse{}, ErrInvalidInput
	}
	if (req.Protocol == "icmp" || req.Protocol == "all") && req.Port != 0 {
		return PolicyExplainResponse{}, ErrInvalidInput
	}

	policy, err := s.policyArtifact(network)
	if err != nil {
		return PolicyExplainResponse{}, err
	}
	devices, err := s.repo.devices(ctx, network.ID)
	if err != nil {
		return PolicyExplainResponse{}, err
	}
	addresses := make(map[string]string, len(devices))
	for _, dev := range devices {
		addresses[dev.ID] = dev.WireGuardAddress
	}

	resp := PolicyExplainResponse{
		NetworkID:          network.ID,
		Revision:           policy.Revision,
		DefaultAction:      policy.DefaultAction,
		Decision:           policy.DefaultAction,
		Reason:             "default_action",
		SourceAddress:      addresses[req.SourceDeviceID],
		DestinationAddress: addresses[req.DestinationDeviceID],
	}

	for i, rule := range policy.Rules {
		if !ruleMatchesDevice(rule.SourceDevices, req.SourceDeviceID) || !ruleMatchesDevice(rule.DestinationDevices, req.DestinationDeviceID) {
			continue
		}
		matched, err := ruleMatchesProtocolAndPort(rule, req.Protocol, req.Port)
		if err != nil {
			return PolicyExplainResponse{}, err
		}
		if matched {
			idx := i
			resp.Decision = rule.Action
			resp.MatchedRuleID = rule.ID
			resp.MatchedRuleIndex = &idx
			resp.Reason = "matched_rule"
			return resp, nil
		}
	}

	return resp, nil
}

func ruleMatchesDevice(refs []string, deviceID string) bool {
	for _, raw := range refs {
		ref := strings.TrimSpace(raw)
		if ref == "*" || ref == deviceID {
			return true
		}
	}
	return false
}

func ruleMatchesProtocolAndPort(rule PolicyRule, queryProto string, queryPort int) (bool, error) {
	ports, err := normalizePolicyPorts(rule.Ports)
	if err != nil {
		return false, err
	}
	protos, err := effectiveRuleProtocols(rule.Protocols, len(ports) > 0)
	if err != nil {
		return false, err
	}

	if queryProto == "all" {
		if slices.Contains(protos, "all") {
			return true, nil
		}
		return len(ports) == 0 && slices.Contains(protos, "tcp") && slices.Contains(protos, "udp") && slices.Contains(protos, "icmp"), nil
	}

	for _, proto := range protos {
		switch proto {
		case "all":
			return true, nil
		case "icmp":
			if queryProto == "icmp" && queryPort == 0 {
				return true, nil
			}
		case "tcp", "udp":
			if queryProto != proto {
				continue
			}
			if len(ports) == 0 || (queryPort > 0 && slices.Contains(ports, queryPort)) {
				return true, nil
			}
		}
	}
	return false, nil
}
