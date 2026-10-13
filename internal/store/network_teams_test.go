package store

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestNetworkTeamCapAndExplicitPersonalRecovery(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	teams := st.(NetworkTeamStore)
	team := NetworkTeam{ID: "acme", Name: "ACME", MemberLimit: 1, Active: true, ValidFrom: time.Now(), ValidUntil: time.Now().Add(time.Hour)}
	if err := teams.PutNetworkTeam(ctx, team, &AuditLog{}); err != nil {
		t.Fatal(err)
	}
	ids := []string{"a", "b"}
	for _, id := range ids {
		if err := st.CreateUser(ctx, &User{ID: id, Name: id, Email: id + "@example.invalid"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := teams.PutNetworkMembership(ctx, NetworkMembership{UserID: ids[0], TeamID: "acme"}, &AuditLog{}); !errors.Is(err, ErrNetworkWithdrawalRequired) {
		t.Fatalf("active team bypassed withdrawal: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range ids {
		if err := teams.PutNetworkMembership(ctx, NetworkMembership{UserID: id, DedicatedOnly: true}, &AuditLog{}); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			errs <- teams.PutNetworkMembership(ctx, NetworkMembership{UserID: id, TeamID: "acme"}, &AuditLog{})
		}(id)
	}
	wg.Wait()
	close(errs)
	full := 0
	for err := range errs {
		if errors.Is(err, ErrNetworkTeamFull) {
			full++
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if full != 1 {
		t.Fatalf("cap race: %d", full)
	}
	var assigned string
	for _, id := range ids {
		m, _ := teams.GetNetworkMembership(ctx, id)
		if m.TeamID != "" {
			assigned = id
		}
	}
	if err := teams.PutNetworkMembership(ctx, NetworkMembership{UserID: assigned, DedicatedOnly: true}, &AuditLog{}); err != nil {
		t.Fatal(err)
	}
	m, _ := teams.GetNetworkMembership(ctx, assigned)
	if !m.DedicatedOnly || m.TeamID != "" {
		t.Fatal("removal restored public access")
	}
	if err := teams.PutNetworkMembership(ctx, NetworkMembership{UserID: assigned}, &AuditLog{}); err != nil {
		t.Fatal(err)
	}
	m, _ = teams.GetNetworkMembership(ctx, assigned)
	if m.DedicatedOnly {
		t.Fatal("explicit personal did not restore access")
	}
}
