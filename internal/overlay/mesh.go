package overlay

import (
	"context"
	"encoding/base64"
	"net/netip"
)

// Initial mesh is intentionally eligible only for whole-device, bidirectional
// grants. An encrypted relay cannot inspect ports, and the current external WG
// adapters have no portable decrypted-packet ACL hook. Restricted pairs remain
// on the existing policy-controlled Gateway path, never silently widened.
func wholeDeviceAllowed(policy PolicyArtifact, source, destination string) bool {
	return grantEvaluator(policy)(source, destination)
}
func grantEvaluator(policy PolicyArtifact) func(string, string) bool {
	masks := make([]uint8, len(policy.Rules))
	for i, rule := range policy.Rules {
		allPorts := len(rule.Ports) == 65535
		for j, p := range rule.Ports {
			if p != j+1 {
				allPorts = false
				break
			}
		}
		for _, protocol := range rule.Protocols {
			switch protocol {
			case "icmp":
				masks[i] |= 1
			case "tcp":
				if allPorts {
					masks[i] |= 2
				}
			case "udp":
				if allPorts {
					masks[i] |= 4
				}
			}
		}
		if rule.WholeDevice && rule.Action == "accept" && masks[i] == 7 {
			masks[i] |= 8
		}
	}
	contains := func(values []string, id string) bool {
		for _, v := range values {
			if v == id {
				return true
			}
		}
		return false
	}
	return func(source, destination string) bool {
		if policy.DefaultAction != "deny" {
			return false
		}
		var mask uint8
		for i, rule := range policy.Rules {
			if !contains(rule.SourceDevices, source) || !contains(rule.DestinationDevices, destination) {
				continue
			}
			if rule.Action == "deny" {
				return false
			}
			if rule.Action == "accept" {
				mask |= masks[i]
			}
		}
		return mask == 15
	}
}
func meshCapable(d DeviceRecord) bool {
	if d.Role != RoleOne || d.Status != "active" || (d.Platform != "linux" && d.Platform != "darwin" && d.Platform != "windows") {
		return false
	}
	key, e := base64.StdEncoding.DecodeString(d.WireGuardPublicKey)
	address, ae := netip.ParsePrefix(d.WireGuardAddress)
	return e == nil && len(key) == 32 && base64.StdEncoding.EncodeToString(key) == d.WireGuardPublicKey && ae == nil && address.Bits() == address.Addr().BitLen() && address.String() == d.WireGuardAddress
}
func (s *Service) meshDevices(ctx context.Context, network NetworkRecord) ([]DeviceRecord, PolicyArtifact, error) {
	devices, e := s.repo.devices(ctx, network.ID)
	if e != nil {
		return nil, PolicyArtifact{}, e
	}
	policy, e := s.policyArtifact(network)
	if e != nil {
		return nil, policy, e
	}
	var result []DeviceRecord
	for _, d := range devices {
		if meshCapable(d) {
			result = append(result, d)
		}
	}
	if len(result) > 256 {
		return nil, policy, ErrInvalidInput
	}
	return result, policy, nil
}
func (s *Service) oneMeshSpec(ctx context.Context, self DeviceRecord, network NetworkRecord) (*MeshSpec, error) {
	if !meshCapable(self) {
		return nil, nil
	}
	devices, policy, e := s.meshDevices(ctx, network)
	if e != nil {
		return nil, e
	}
	allowed := grantEvaluator(policy)
	spec := &MeshSpec{}
	for _, d := range devices {
		if d.ID != self.ID && d.WireGuardPublicKey != self.WireGuardPublicKey && allowed(self.ID, d.ID) && allowed(d.ID, self.ID) {
			spec.Peers = append(spec.Peers, MeshPeer{DeviceID: d.ID, PublicKey: d.WireGuardPublicKey, Address: d.WireGuardAddress})
		}
	}
	if len(spec.Peers) == 0 {
		return nil, nil
	}
	return spec, nil
}
func (s *Service) relaySpec(ctx context.Context, network NetworkRecord) (*RelaySpec, error) {
	devices, policy, e := s.meshDevices(ctx, network)
	if e != nil {
		return nil, e
	}
	allowed := grantEvaluator(policy)
	spec := &RelaySpec{}
	for _, a := range devices {
		p := RelayPeer{DeviceID: a.ID, PublicKey: a.WireGuardPublicKey}
		for _, b := range devices {
			if a.ID != b.ID && a.WireGuardPublicKey != b.WireGuardPublicKey && allowed(a.ID, b.ID) && allowed(b.ID, a.ID) {
				p.AllowedPeers = append(p.AllowedPeers, b.ID)
			}
		}
		if len(p.AllowedPeers) > 0 {
			spec.Peers = append(spec.Peers, p)
		}
	}
	if len(spec.Peers) == 0 {
		return nil, nil
	}
	return spec, nil
}
