package migrate

import (
	"testing"
	"time"
)

func TestValidateGenerationPairRequiresAdjacentVersions(t *testing.T) {
	for _, pair := range [][2]string{{"prod-v2", "prod-v3"}, {"prod-v9", "prod-v10"}} {
		if err := validateGenerationPair(pair[0], pair[1]); err != nil {
			t.Fatalf("expected adjacent generation %v to pass: %v", pair, err)
		}
	}
	for _, pair := range [][2]string{{"prod-v2", "prod-v4"}, {"prod-v3", "uat-v4"}, {"prod", "prod-v2"}, {"prod-v0", "prod-v1"}} {
		if err := validateGenerationPair(pair[0], pair[1]); err == nil {
			t.Fatalf("expected generation pair %v to fail", pair)
		}
	}
}

func TestGenerationUserBaselineIsBoundToPairAndChecksummed(t *testing.T) {
	options := GenerationUserOptions{SourceGeneration: "prod-v2", TargetGeneration: "prod-v3"}
	baseline := GenerationUserBaseline{
		Format: 1, Algorithm: "sha256-generation-user-id-v1",
		SourceGeneration: "prod-v2", TargetGeneration: "prod-v3",
		CapturedAt:      time.Date(2026, 10, 19, 0, 0, 0, 0, time.UTC),
		SourceUserCount: 1, TargetUserCount: 1,
		SourceUserIDHashes: []string{generationUserIDHash("00000000-0000-0000-0000-000000000001")},
		IdentityDiff:       GenerationUserDiff{Equal: true},
	}
	baseline.SourceUserSetSHA256 = hashString(baseline.SourceUserIDHashes[0])
	baseline.BaselineSHA256, _ = baselineChecksum(baseline)
	if err := validateGenerationUserBaseline(baseline, options); err != nil {
		t.Fatalf("valid baseline rejected: %v", err)
	}
	baseline.TargetGeneration = "prod-v4"
	if err := validateGenerationUserBaseline(baseline, options); err == nil {
		t.Fatal("baseline from another generation pair was accepted")
	}
}

func TestGenerationUserSetUsesStableIDsNotRegistrationTime(t *testing.T) {
	users := map[string]businessUser{
		"same@example.invalid": {ID: "00000000-0000-0000-0000-000000000001", Email: "same@example.invalid"},
	}
	hashes, setHash := generationUserSet(users)
	if len(hashes) != 1 || hashes[0] != generationUserIDHash(users["same@example.invalid"].ID) || setHash == "" {
		t.Fatal("stable source UUID baseline was not produced")
	}
	late := businessUser{ID: "00000000-0000-0000-0000-000000000002", Email: "late@example.invalid"}
	if generationUserIDHash(late.ID) == hashes[0] {
		t.Fatal("different source UUID was hidden by an old registration timestamp")
	}
}
