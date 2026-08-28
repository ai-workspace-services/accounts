package acl

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	APIVersion      = "overlay.xconnect.svc.plus/v1alpha1"
	Kind            = "NetworkPolicy"
	CompilerVersion = "xconnect-acl-v1alpha1.1"
	MaxDocumentSize = 1 << 20
)

type Document struct {
	APIVersion string   `json:"apiVersion" yaml:"apiVersion"`
	Kind       string   `json:"kind" yaml:"kind"`
	Metadata   Metadata `json:"metadata" yaml:"metadata"`
	Spec       Spec     `json:"spec" yaml:"spec"`
}
type Metadata struct {
	Name string `json:"name" yaml:"name"`
}
type Spec struct {
	DefaultAction string                    `json:"defaultAction" yaml:"defaultAction"`
	Groups        map[string]Group          `json:"groups,omitempty" yaml:"groups,omitempty"`
	TagOwners     map[string][]string       `json:"tagOwners,omitempty" yaml:"tagOwners,omitempty"`
	Services      map[string]NetworkService `json:"services,omitempty" yaml:"services,omitempty"`
	Rules         []Rule                    `json:"rules,omitempty" yaml:"rules,omitempty"`
}
type Group struct {
	Users []string `json:"users" yaml:"users"`
}
type NetworkService struct {
	Destinations []string `json:"destinations" yaml:"destinations"`
	Protocols    []string `json:"protocols" yaml:"protocols"`
	Ports        []int    `json:"ports" yaml:"ports"`
}
type Rule struct {
	ID           string   `json:"id" yaml:"id"`
	Action       string   `json:"action" yaml:"action"`
	Sources      []string `json:"sources" yaml:"sources"`
	Destinations []string `json:"destinations" yaml:"destinations"`
	Protocols    []string `json:"protocols,omitempty" yaml:"protocols,omitempty"`
	Ports        []int    `json:"ports,omitempty" yaml:"ports,omitempty"`
}

// Inventory is a point-in-time controller view used for fail-closed subject resolution.
type Inventory struct {
	Users        map[string]string   `json:"users"`
	Devices      map[string][]string `json:"devices"`
	DeviceOwners map[string]string   `json:"device_owners"`
}

type Warning struct{ Code, RuleID, Message string }
type IRRule struct {
	ID                 string   `json:"id"`
	Action             string   `json:"action"`
	Sources            []string `json:"sources"`
	Destinations       []string `json:"destinations"`
	Protocols          []string `json:"protocols"`
	Ports              []int    `json:"ports"`
	SourceDevices      []string `json:"source_devices"`
	DestinationDevices []string `json:"destination_devices"`
}
type DevicePrincipal struct {
	OwnerUserID string   `json:"owner_user_id"`
	Owner       string   `json:"owner"`
	Tags        []string `json:"tags"`
}
type Artifact struct {
	SchemaVersion   int                        `json:"schema_version"`
	CompilerVersion string                     `json:"compiler_version"`
	NetworkID       string                     `json:"network_id"`
	Revision        uint64                     `json:"revision"`
	DefaultAction   string                     `json:"default_action"`
	ProtectedFlows  []string                   `json:"protected_flows"`
	Groups          map[string][]string        `json:"groups"`
	Users           map[string]string          `json:"users"`
	Devices         map[string][]string        `json:"devices"`
	DeviceOwners    map[string]string          `json:"device_owners"`
	Principals      map[string]DevicePrincipal `json:"principals"`
	Services        map[string]NetworkService  `json:"services"`
	TagOwners       map[string][]string        `json:"tag_owners"`
	Rules           []IRRule                   `json:"rules"`
}
type Build struct {
	Artifact  Artifact
	Canonical []byte
	Digest    string
	Warnings  []Warning
}
type EnforcementRule struct {
	ID                 string   `json:"id"`
	Action             string   `json:"action"`
	SourceDevices      []string `json:"source_devices"`
	DestinationDevices []string `json:"destination_devices"`
	Protocols          []string `json:"protocols"`
	Ports              []int    `json:"ports"`
}
type EnforcementArtifact struct {
	SchemaVersion   int               `json:"schema_version"`
	CompilerVersion string            `json:"compiler_version"`
	NetworkID       string            `json:"network_id"`
	Revision        uint64            `json:"revision"`
	DefaultAction   string            `json:"default_action"`
	ProtectedFlows  []string          `json:"protected_flows"`
	Rules           []EnforcementRule `json:"rules"`
}

func Parse(raw []byte, contentType string) (Document, error) {
	if len(raw) == 0 || len(raw) > MaxDocumentSize {
		return Document{}, errors.New("policy document size is invalid")
	}
	var doc Document
	if strings.Contains(strings.ToLower(contentType), "yaml") {
		var root yaml.Node
		if err := yaml.Unmarshal(raw, &root); err != nil {
			return Document{}, fmt.Errorf("strict YAML: %w", err)
		}
		if invalidYAMLNode(&root) {
			return Document{}, errors.New("YAML aliases and merge keys are not allowed")
		}
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&doc); err != nil {
			return Document{}, fmt.Errorf("strict YAML: %w", err)
		}
		var extra any
		if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
			return Document{}, errors.New("policy must contain exactly one YAML document")
		}
	} else {
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&doc); err != nil {
			return Document{}, fmt.Errorf("strict JSON: %w", err)
		}
		if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return Document{}, errors.New("policy must contain exactly one JSON object")
		}
	}
	return doc, nil
}
func invalidYAMLNode(node *yaml.Node) bool {
	if node == nil {
		return false
	}
	if node.Kind == yaml.AliasNode || node.Value == "<<" {
		return true
	}
	for _, child := range node.Content {
		if invalidYAMLNode(child) {
			return true
		}
	}
	return false
}

func Compile(doc Document, networkID string, revision uint64, inv Inventory) (Build, error) {
	if doc.APIVersion != APIVersion || doc.Kind != Kind || strings.TrimSpace(doc.Metadata.Name) == "" {
		return Build{}, errors.New("invalid NetworkPolicy identity")
	}
	if doc.Spec.DefaultAction != "deny" {
		return Build{}, errors.New("spec.defaultAction must be deny")
	}
	if strings.TrimSpace(networkID) == "" || revision == 0 {
		return Build{}, errors.New("network and revision are required")
	}
	users := normalizedMap(inv.Users)
	devices := normalizedSliceMap(inv.Devices)
	deviceOwners := map[string]string{}
	principals := map[string]DevicePrincipal{}
	emailByID := map[string]string{}
	for email, userID := range users {
		emailByID[userID] = email
	}
	for deviceID, ownerID := range inv.DeviceOwners {
		deviceID = strings.TrimSpace(deviceID)
		ownerID = strings.TrimSpace(ownerID)
		email := emailByID[ownerID]
		if email == "" {
			return Build{}, fmt.Errorf("device %s has unknown owner", deviceID)
		}
		owner := "user:" + email
		deviceOwners[deviceID] = owner
		principals[deviceID] = DevicePrincipal{OwnerUserID: ownerID, Owner: owner, Tags: uniqueSorted(devices[deviceID])}
	}
	for deviceID := range devices {
		if _, ok := principals[deviceID]; !ok {
			return Build{}, fmt.Errorf("device %s has no owner mapping", deviceID)
		}
	}
	tags := map[string]bool{}
	for _, values := range devices {
		for _, tag := range values {
			tags[strings.ToLower(tag)] = true
		}
	}
	for tag := range doc.Spec.TagOwners {
		tags[strings.ToLower(strings.TrimSpace(tag))] = true
	}
	groups := map[string][]string{}
	for rawName, group := range doc.Spec.Groups {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !validName(name) {
			return Build{}, fmt.Errorf("invalid group %q", name)
		}
		for _, rawEmail := range uniqueSorted(group.Users) {
			email := strings.ToLower(rawEmail)
			if users[email] == "" {
				return Build{}, fmt.Errorf("unknown subject user:%s", email)
			}
			groups[name] = append(groups[name], email)
		}
	}
	tagOwners := map[string][]string{}
	for rawTag, owners := range doc.Spec.TagOwners {
		tag := strings.ToLower(strings.TrimSpace(rawTag))
		if !strings.HasPrefix(tag, "tag:") || len(owners) == 0 {
			return Build{}, fmt.Errorf("invalid tag owner %q", tag)
		}
		for _, rawOwner := range uniqueSorted(owners) {
			owner := normalizeSelector(rawOwner)
			if !strings.HasPrefix(owner, "user:") && !strings.HasPrefix(owner, "group:") {
				return Build{}, fmt.Errorf("tag owner %s must be user or group", tag)
			}
			if err := validateSelector(owner, false, users, groups, devices, tags, doc.Spec.Services); err != nil {
				return Build{}, fmt.Errorf("tag owner %s: %w", tag, err)
			}
			tagOwners[tag] = append(tagOwners[tag], owner)
		}
	}
	services := map[string]NetworkService{}
	for rawName, service := range doc.Spec.Services {
		name := strings.ToLower(strings.TrimSpace(rawName))
		if !validName(name) {
			return Build{}, fmt.Errorf("invalid service %q", name)
		}
		p, err := normalizeProtocols(service.Protocols)
		if err != nil {
			return Build{}, fmt.Errorf("service %s: %w", name, err)
		}
		ports, err := normalizePorts(service.Ports, p)
		if err != nil {
			return Build{}, fmt.Errorf("service %s: %w", name, err)
		}
		service.Destinations = normalizedSelectors(service.Destinations)
		if len(service.Destinations) == 0 {
			return Build{}, fmt.Errorf("service %s requires destinations", name)
		}
		for _, selector := range service.Destinations {
			if strings.HasPrefix(selector, "service:") {
				return Build{}, fmt.Errorf("service %s cannot reference another service", name)
			}
			if err := validateSelector(selector, true, users, groups, devices, tags, services); err != nil {
				return Build{}, fmt.Errorf("service %s: %w", name, err)
			}
		}
		service.Protocols, service.Ports = p, ports
		services[name] = service
	}
	seen := map[string]bool{}
	rules := make([]IRRule, 0, len(doc.Spec.Rules))
	warnings := []Warning{}
	for _, input := range doc.Spec.Rules {
		if !validName(input.ID) || seen[input.ID] {
			return Build{}, fmt.Errorf("invalid or duplicate rule id %q", input.ID)
		}
		seen[input.ID] = true
		if input.Action != "accept" && input.Action != "deny" {
			return Build{}, fmt.Errorf("rule %s has invalid action", input.ID)
		}
		if len(input.Sources) == 0 || len(input.Destinations) == 0 {
			return Build{}, fmt.Errorf("rule %s requires sources and destinations", input.ID)
		}
		src, dst := normalizedSelectors(input.Sources), normalizedSelectors(input.Destinations)
		for _, s := range src {
			if err := validateSelector(s, false, users, groups, devices, tags, services); err != nil {
				return Build{}, fmt.Errorf("rule %s: %w", input.ID, err)
			}
		}
		for _, s := range dst {
			if err := validateSelector(s, true, users, groups, devices, tags, services); err != nil {
				return Build{}, fmt.Errorf("rule %s: %w", input.ID, err)
			}
		}
		protocolInput, portInput := input.Protocols, input.Ports
		serviceName := ""
		for _, selector := range dst {
			if strings.HasPrefix(selector, "service:") {
				if serviceName != "" || len(dst) != 1 {
					return Build{}, fmt.Errorf("rule %s service destination must be the only destination", input.ID)
				}
				serviceName = strings.TrimPrefix(selector, "service:")
			}
		}
		if serviceName != "" {
			service := services[serviceName]
			if len(protocolInput) == 0 {
				protocolInput = service.Protocols
			} else {
				protocolInput = intersectStrings(protocolInput, service.Protocols)
			}
			if len(portInput) == 0 {
				portInput = service.Ports
			} else {
				portInput = intersectInts(portInput, service.Ports)
			}
			if len(protocolInput) == 0 || (!contains(protocolInput, "icmp") && len(portInput) == 0) {
				return Build{}, fmt.Errorf("rule %s does not intersect service %s", input.ID, serviceName)
			}
		}
		protocols, err := normalizeProtocols(protocolInput)
		if err != nil {
			return Build{}, fmt.Errorf("rule %s: %w", input.ID, err)
		}
		ports, err := normalizePorts(portInput, protocols)
		if err != nil {
			return Build{}, fmt.Errorf("rule %s: %w", input.ID, err)
		}
		sourceDevices := expandDevices(src, groups, devices, deviceOwners, services)
		destinationDevices := expandDevices(dst, groups, devices, deviceOwners, services)
		rule := IRRule{ID: input.ID, Action: input.Action, Sources: src, Destinations: dst, Protocols: protocols, Ports: ports, SourceDevices: sourceDevices, DestinationDevices: destinationDevices}
		rules = append(rules, rule)
	}
	// Stable deny-first ordering is part of the cross-runtime contract.
	sort.SliceStable(rules, func(i, j int) bool {
		if rules[i].Action != rules[j].Action {
			return rules[i].Action == "deny"
		}
		return rules[i].ID < rules[j].ID
	})
	for i, rule := range rules {
		for j := 0; j < i; j++ {
			previous := rules[j]
			if selectorScopeShadows(previous, rule) {
				warnings = append(warnings, Warning{Code: "shadowed_rule", RuleID: rule.ID, Message: "rule is fully shadowed by " + previous.ID})
				break
			}
		}
	}
	artifact := Artifact{SchemaVersion: 1, CompilerVersion: CompilerVersion, NetworkID: networkID, Revision: revision, DefaultAction: "deny", ProtectedFlows: []string{"control:controller-session", "control:gateway-apply-result", "control:gateway-heartbeat", "control:gateway-policy-artifact", "control:gateway-snapshot"}, Groups: groups, Users: users, Devices: devices, DeviceOwners: deviceOwners, Principals: principals, Services: services, TagOwners: tagOwners, Rules: rules}
	enforcementRules := make([]EnforcementRule, 0, len(rules))
	for _, rule := range rules {
		if len(rule.SourceDevices) == 0 || len(rule.DestinationDevices) == 0 {
			continue
		}
		enforcementRules = append(enforcementRules, EnforcementRule{ID: rule.ID, Action: rule.Action, SourceDevices: rule.SourceDevices, DestinationDevices: rule.DestinationDevices, Protocols: rule.Protocols, Ports: rule.Ports})
	}
	enforcement := EnforcementArtifact{SchemaVersion: 1, CompilerVersion: CompilerVersion, NetworkID: networkID, Revision: revision, DefaultAction: "deny", ProtectedFlows: artifact.ProtectedFlows, Rules: enforcementRules}
	canonical, err := json.Marshal(enforcement)
	if err != nil {
		return Build{}, err
	}
	sum := sha256.Sum256(canonical)
	return Build{Artifact: artifact, Canonical: canonical, Digest: hex.EncodeToString(sum[:]), Warnings: warnings}, nil
}

type Query struct {
	Source, Destination, Protocol string
	Port                          int
}
type Explanation struct {
	Action                     string   `json:"action"`
	RuleID                     string   `json:"rule_id,omitempty"`
	Reason                     string   `json:"reason"`
	Protected                  bool     `json:"protected"`
	ResolvedSourceDevices      []string `json:"resolved_source_devices,omitempty"`
	ResolvedDestinationDevices []string `json:"resolved_destination_devices,omitempty"`
}

func Explain(a Artifact, q Query) Explanation {
	q.Source = normalizeSelector(q.Source)
	q.Destination = normalizeSelector(q.Destination)
	q.Protocol = strings.ToLower(strings.TrimSpace(q.Protocol))
	if contains(a.ProtectedFlows, q.Destination) {
		return Explanation{Action: "accept", RuleID: "_xconnect_control_plane", Reason: "protected control-plane flow", Protected: true}
	}
	for _, r := range a.Rules {
		if matches(r.Sources, q.Source, a) && matches(r.Destinations, q.Destination, a) && matchesProtocolPort(r, q.Protocol, q.Port) {
			return Explanation{Action: r.Action, RuleID: r.ID, Reason: "matched canonical rule", ResolvedSourceDevices: append([]string(nil), r.SourceDevices...), ResolvedDestinationDevices: append([]string(nil), r.DestinationDevices...)}
		}
	}
	return Explanation{Action: "deny", Reason: "default deny"}
}

func matches(selectors []string, value string, a Artifact) bool {
	for _, s := range selectors {
		if s == value {
			return true
		}
		if strings.HasPrefix(s, "service:") && matches(a.Services[strings.TrimPrefix(s, "service:")].Destinations, value, a) {
			return true
		}
		if strings.HasPrefix(value, "device:") {
			id := strings.TrimPrefix(value, "device:")
			owner := a.DeviceOwners[id]
			if strings.HasPrefix(s, "user:") && s == owner {
				return true
			}
			if strings.HasPrefix(s, "group:") {
				for _, email := range a.Groups[strings.TrimPrefix(s, "group:")] {
					if owner == "user:"+email {
						return true
					}
				}
			}
			if strings.HasPrefix(s, "tag:") {
				for _, tag := range a.Devices[id] {
					if s == strings.ToLower(tag) {
						return true
					}
				}
			}
		}
	}
	return false
}
func matchesProtocolPort(r IRRule, p string, port int) bool {
	if !contains(r.Protocols, p) {
		return false
	}
	if p == "icmp" {
		return port == 0
	}
	for _, v := range r.Ports {
		if v == port {
			return true
		}
	}
	return false
}
func validateSelector(s string, destination bool, users map[string]string, groups map[string][]string, devices map[string][]string, tags map[string]bool, services map[string]NetworkService) error {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 || parts[1] == "" {
		return fmt.Errorf("invalid subject %q", s)
	}
	switch parts[0] {
	case "user":
		if users[parts[1]] == "" {
			return fmt.Errorf("unknown subject %s", s)
		}
	case "group":
		if _, ok := groups[parts[1]]; !ok {
			return fmt.Errorf("unknown subject %s", s)
		}
	case "device":
		if _, ok := devices[parts[1]]; !ok {
			return fmt.Errorf("unknown subject %s", s)
		}
	case "tag":
		if !tags[s] {
			return fmt.Errorf("unknown subject %s", s)
		}
	case "service":
		if !destination {
			return fmt.Errorf("service is destination-only")
		}
		if _, ok := services[parts[1]]; !ok {
			return fmt.Errorf("unknown subject %s", s)
		}
	default:
		return fmt.Errorf("unknown subject %s", s)
	}
	return nil
}
func normalizeProtocols(in []string) ([]string, error) {
	if len(in) == 0 {
		return []string{"tcp", "udp"}, nil
	}
	out := uniqueSorted(in)
	for _, p := range out {
		if p != "tcp" && p != "udp" && p != "icmp" {
			return nil, fmt.Errorf("invalid protocol %q", p)
		}
	}
	return out, nil
}
func normalizePorts(in []int, protocols []string) ([]int, error) {
	if contains(protocols, "icmp") {
		if len(protocols) > 1 || len(in) > 0 {
			return nil, errors.New("icmp cannot be combined with ports or other protocols")
		}
		return []int{}, nil
	}
	if len(in) == 0 {
		return nil, errors.New("tcp/udp rules require ports")
	}
	sort.Ints(in)
	out := in[:0]
	last := -1
	for _, p := range in {
		if p < 1 || p > 65535 {
			return nil, fmt.Errorf("invalid port %s", strconv.Itoa(p))
		}
		if p != last {
			out = append(out, p)
			last = p
		}
	}
	return out, nil
}
func CanAssignTag(a Artifact, actor, tag string) bool {
	actor = normalizeSelector(actor)
	tag = strings.ToLower(strings.TrimSpace(tag))
	for _, owner := range a.TagOwners[tag] {
		if owner == actor {
			return true
		}
		if strings.HasPrefix(owner, "group:") && strings.HasPrefix(actor, "user:") {
			for _, email := range a.Groups[strings.TrimPrefix(owner, "group:")] {
				if actor == "user:"+email {
					return true
				}
			}
		}
	}
	return false
}
func selectorScopeShadows(a, b IRRule) bool {
	return (a.Action == b.Action || a.Action == "deny") && subset(b.SourceDevices, a.SourceDevices) && subset(b.DestinationDevices, a.DestinationDevices) && subset(b.Protocols, a.Protocols) && subsetInts(b.Ports, a.Ports)
}
func subset(a, b []string) bool {
	for _, v := range a {
		if !contains(b, v) {
			return false
		}
	}
	return true
}
func subsetInts(a, b []int) bool {
	for _, v := range a {
		ok := false
		for _, x := range b {
			if x == v {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
func contains(a []string, v string) bool {
	for _, x := range a {
		if x == v {
			return true
		}
	}
	return false
}
func uniqueSorted(in []string) []string {
	m := map[string]bool{}
	for _, v := range in {
		v = strings.TrimSpace(v)
		if v != "" {
			m[v] = true
		}
	}
	out := make([]string, 0, len(m))
	for v := range m {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}
func normalizeSelector(v string) string {
	parts := strings.SplitN(strings.TrimSpace(v), ":", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(v)
	}
	prefix := strings.ToLower(parts[0])
	value := strings.TrimSpace(parts[1])
	if prefix == "user" || prefix == "group" || prefix == "tag" || prefix == "service" {
		value = strings.ToLower(value)
	}
	return prefix + ":" + value
}
func normalizedSelectors(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		out = append(out, normalizeSelector(v))
	}
	return uniqueSorted(out)
}
func intersectStrings(a, b []string) []string {
	aa := uniqueSorted(a)
	out := []string{}
	for _, v := range aa {
		if contains(b, v) {
			out = append(out, v)
		}
	}
	return out
}
func intersectInts(a, b []int) []int {
	out := []int{}
	for _, v := range a {
		for _, x := range b {
			if v == x {
				out = append(out, v)
				break
			}
		}
	}
	return out
}
func expandDevices(selectors []string, groups map[string][]string, devices map[string][]string, owners map[string]string, services map[string]NetworkService) []string {
	set := map[string]bool{}
	var add func(string)
	add = func(selector string) {
		switch {
		case strings.HasPrefix(selector, "device:"):
			id := strings.TrimPrefix(selector, "device:")
			if _, ok := devices[id]; ok {
				set[id] = true
			}
		case strings.HasPrefix(selector, "user:"):
			for id, owner := range owners {
				if owner == selector {
					set[id] = true
				}
			}
		case strings.HasPrefix(selector, "group:"):
			for _, email := range groups[strings.TrimPrefix(selector, "group:")] {
				add("user:" + email)
			}
		case strings.HasPrefix(selector, "tag:"):
			for id, tags := range devices {
				for _, tag := range tags {
					if strings.ToLower(tag) == selector {
						set[id] = true
					}
				}
			}
		case strings.HasPrefix(selector, "service:"):
			for _, target := range services[strings.TrimPrefix(selector, "service:")].Destinations {
				add(target)
			}
		}
	}
	for _, selector := range selectors {
		add(selector)
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
func normalizedMap(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
	}
	return out
}
func normalizedSliceMap(in map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range in {
		out[strings.TrimSpace(k)] = uniqueSorted(v)
	}
	return out
}
func validName(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, r := range s {
		if !(r == '-' || r == '_' || r == '.' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
