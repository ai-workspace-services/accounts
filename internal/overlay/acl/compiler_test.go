package acl

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture() (Document, Inventory) {
	doc := Document{APIVersion: APIVersion, Kind: Kind, Metadata: Metadata{Name: "private"}, Spec: Spec{DefaultAction: "deny", Groups: map[string]Group{"Developers": {Users: []string{"ALICE@example.com"}}}, TagOwners: map[string][]string{"tag:gateway": {"group:developers"}}, Services: map[string]NetworkService{"API": {Destinations: []string{"tag:gateway"}, Protocols: []string{"tcp"}, Ports: []int{8787}}}, Rules: []Rule{{ID: "allow-api", Action: "accept", Sources: []string{"group:DEVELOPERS"}, Destinations: []string{"service:API"}}}}}
	inv := Inventory{Users: map[string]string{"alice@example.com": "user-1", "bob@example.com": "user-2"}, Devices: map[string][]string{"dev-a": {"tag:client"}, "dev-b": {"tag:gateway"}}, DeviceOwners: map[string]string{"dev-a": "user-1", "dev-b": "user-2"}}
	return doc, inv
}

func TestACLCases001002005007009010FamilyNeutral(t *testing.T) {
	doc, inv := fixture()
	build, err := Compile(doc, "net-v4-v6", 1, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got := Explain(build.Artifact, Query{Source: "device:dev-a", Destination: "device:dev-b", Protocol: "tcp", Port: 8787}); got.Action != "accept" || got.RuleID != "allow-api" {
		t.Fatalf("ACL-002/010: %#v", got)
	}
	if got := Explain(build.Artifact, Query{Source: "device:dev-a", Destination: "device:dev-b", Protocol: "tcp", Port: 443}); got.Action != "deny" {
		t.Fatalf("ACL-001: %#v", got)
	}
	if got := Explain(build.Artifact, Query{Destination: "control:unknown"}); got.Action != "deny" {
		t.Fatalf("unknown control bypass: %#v", got)
	}
	if got := Explain(build.Artifact, Query{Destination: "control:gateway-heartbeat"}); !got.Protected || got.Action != "accept" {
		t.Fatalf("ACL-009: %#v", got)
	}
	var runtime EnforcementArtifact
	if err = json.Unmarshal(build.Canonical, &runtime); err != nil {
		t.Fatal(err)
	}
	text := string(build.Canonical)
	if strings.Contains(text, "alice@example.com") || strings.Contains(text, "user-1") || strings.Contains(text, "tag:gateway") {
		t.Fatalf("runtime artifact contains management graph/PII: %s", text)
	}
	if len(runtime.Rules) != 1 || fmt.Sprint(runtime.Rules[0].SourceDevices) != "[dev-a]" || fmt.Sprint(runtime.Rules[0].DestinationDevices) != "[dev-b]" {
		t.Fatalf("principal expansion: %#v", runtime.Rules)
	}
	moved := inv
	moved.DeviceOwners = map[string]string{"dev-a": "user-2", "dev-b": "user-2"}
	movedBuild, err := Compile(doc, "net-v4-v6", 1, moved)
	if err != nil {
		t.Fatal(err)
	}
	if movedBuild.Digest == build.Digest {
		t.Fatal("ACL-005 membership change did not change artifact")
	}
	var movedRuntime EnforcementArtifact
	if err = json.Unmarshal(movedBuild.Canonical, &movedRuntime); err != nil || len(movedRuntime.Rules) != 0 {
		t.Fatalf("empty membership rule must be omitted: %#v %v", movedRuntime.Rules, err)
	}
	if got := Explain(movedBuild.Artifact, Query{Source: "device:dev-a", Destination: "device:dev-b", Protocol: "tcp", Port: 8787}); got.Action != "deny" {
		t.Fatalf("ACL-005 removed member retained access: %#v", got)
	}
}

func TestACL003008DenyFirstShadowWarning(t *testing.T) {
	doc, inv := fixture()
	doc.Spec.Rules = []Rule{{ID: "allow", Action: "accept", Sources: []string{"group:developers"}, Destinations: []string{"tag:gateway"}, Protocols: []string{"tcp"}, Ports: []int{443}}, {ID: "deny", Action: "deny", Sources: []string{"user:alice@example.com"}, Destinations: []string{"device:dev-b"}, Protocols: []string{"tcp"}, Ports: []int{443}}}
	build, err := Compile(doc, "net", 1, inv)
	if err != nil {
		t.Fatal(err)
	}
	if build.Artifact.Rules[0].ID != "deny" || len(build.Warnings) != 1 || build.Warnings[0].RuleID != "allow" {
		t.Fatalf("deny ordering/warning: %#v %#v", build.Artifact.Rules, build.Warnings)
	}
	if got := Explain(build.Artifact, Query{Source: "device:dev-a", Destination: "device:dev-b", Protocol: "tcp", Port: 443}); got.Action != "deny" {
		t.Fatalf("ACL-003: %#v", got)
	}
}

func TestACL004TagOwnerAssignment(t *testing.T) {
	doc, inv := fixture()
	build, err := Compile(doc, "net", 1, inv)
	if err != nil {
		t.Fatal(err)
	}
	if !CanAssignTag(build.Artifact, "user:ALICE@example.com", "tag:gateway") || CanAssignTag(build.Artifact, "user:bob@example.com", "tag:gateway") {
		t.Fatal("tag owner authorization failed")
	}
	doc.Spec.TagOwners["tag:gateway"] = []string{"device:dev-a"}
	if _, err = Compile(doc, "net", 1, inv); err == nil {
		t.Fatal("device tag owner accepted")
	}
}

func TestStrictParserAndUnknownSubjects(t *testing.T) {
	bad := []byte("apiVersion: overlay.xconnect.svc.plus/v1alpha1\nkind: NetworkPolicy\nmetadata: {name: x, name: y}\nspec: {defaultAction: deny}\n")
	if _, err := Parse(bad, "application/yaml"); err == nil {
		t.Fatal("duplicate YAML key accepted")
	}
	alias := []byte("apiVersion: overlay.xconnect.svc.plus/v1alpha1\nkind: NetworkPolicy\nmetadata: &m {name: x}\nspec: {defaultAction: deny}\nextra: *m\n")
	if _, err := Parse(alias, "application/yaml"); err == nil {
		t.Fatal("YAML alias accepted")
	}
	doc, inv := fixture()
	doc.Spec.Rules[0].Sources = []string{"user:nobody@example.com"}
	if _, err := Compile(doc, "net", 1, inv); err == nil {
		t.Fatal("ACL-007 unknown subject accepted")
	}
	raw := []byte(`{"apiVersion":"overlay.xconnect.svc.plus/v1alpha1","kind":"NetworkPolicy","metadata":{"name":"x"},"spec":{"defaultAction":"deny","private_key":"x"}}`)
	if _, err := Parse(raw, "application/json"); err == nil {
		t.Fatal("secret/unknown field accepted")
	}
}

func TestDeterministicGolden(t *testing.T) {
	doc, inv := fixture()
	first, err := Compile(doc, "network-golden", 7, inv)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Compile(doc, "network-golden", 7, inv)
	if err != nil || first.Digest != second.Digest || string(first.Canonical) != string(second.Canonical) {
		t.Fatal("compiler is not deterministic")
	}
	const want = "58941760a9ab4568d2e72a6f34a2cede891d8e678346da8e886d86263e5b780c"
	fixtureRaw, readErr := os.ReadFile(filepath.Join("..", "..", "..", "tests", "fixtures", "overlay", "network-policy-enforcement.golden.json"))
	if readErr != nil || string(first.Canonical) != strings.TrimSpace(string(fixtureRaw)) {
		t.Fatalf("golden bytes drift: %v", readErr)
	}
	if first.Digest != want {
		t.Fatalf("golden digest = %s", first.Digest)
	}
}

func TestScaleSmoke10kDevices1kRules(t *testing.T) {
	started := time.Now()
	doc, inv := fixture()
	doc.Spec.Services = nil
	doc.Spec.Rules = make([]Rule, 1000)
	inv.Devices = map[string][]string{}
	inv.DeviceOwners = map[string]string{}
	for i := 0; i < 10000; i++ {
		id := fmt.Sprintf("dev-%05d", i)
		inv.Devices[id] = []string{"tag:fleet"}
		inv.DeviceOwners[id] = "user-1"
	}
	for i := range doc.Spec.Rules {
		doc.Spec.Rules[i] = Rule{ID: fmt.Sprintf("rule-%04d", i), Action: "accept", Sources: []string{fmt.Sprintf("device:dev-%05d", i)}, Destinations: []string{"device:dev-09999"}, Protocols: []string{"tcp"}, Ports: []int{443}}
	}
	build, err := Compile(doc, "scale", 1, inv)
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(started) > 5*time.Second || len(build.Canonical) > 2<<20 {
		t.Fatalf("scale budget exceeded: duration=%s bytes=%d", time.Since(started), len(build.Canonical))
	}
}

func TestServiceICMPAllowsEmptyPorts(t *testing.T) {
	doc, inv := fixture()
	doc.Spec.Services = map[string]NetworkService{"ping": {Destinations: []string{"device:dev-b"}, Protocols: []string{"icmp"}, Ports: []int{}}}
	doc.Spec.Rules = []Rule{{ID: "ping", Action: "accept", Sources: []string{"device:dev-a"}, Destinations: []string{"service:ping"}}}
	build, err := Compile(doc, "net", 1, inv)
	if err != nil {
		t.Fatal(err)
	}
	if got := Explain(build.Artifact, Query{Source: "device:dev-a", Destination: "device:dev-b", Protocol: "icmp", Port: 0}); got.Action != "accept" {
		t.Fatalf("icmp service: %#v", got)
	}
}

func FuzzParseNetworkPolicy(f *testing.F) {
	f.Add([]byte(`{"apiVersion":"overlay.xconnect.svc.plus/v1alpha1","kind":"NetworkPolicy","metadata":{"name":"x"},"spec":{"defaultAction":"deny"}}`))
	f.Fuzz(func(t *testing.T, raw []byte) { _, _ = Parse(raw, "application/json") })
}
