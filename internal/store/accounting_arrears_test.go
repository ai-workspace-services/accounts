package store

import (
	"context"
	"testing"
	"time"
)

func TestAccountArrearsPromotesToSuspendedAfterFourteenDaysAndPaidClearsIt(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()
	user := &User{Name: "arrears user", Email: "arrears@example.test", Active: true}
	if err := s.CreateUser(ctx, user); err != nil {
		t.Fatalf("create user: %v", err)
	}
	failedAt := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	if err := s.MarkAccountArrears(ctx, user.ID, failedAt); err != nil {
		t.Fatalf("mark arrears: %v", err)
	}
	// A retry must not reset the grace period.
	if err := s.MarkAccountArrears(ctx, user.ID, failedAt.Add(7*24*time.Hour)); err != nil {
		t.Fatalf("mark retry: %v", err)
	}
	state, err := s.GetAccountQuotaState(ctx, user.ID)
	if err != nil || state.ArrearsSince == nil || !state.ArrearsSince.Equal(failedAt) {
		t.Fatalf("expected first failure timestamp to be retained, state=%+v err=%v", state, err)
	}
	if suspended, err := s.IsAccountSuspended(ctx, user.ID, failedAt.Add(14*24*time.Hour-time.Second)); err != nil || suspended {
		t.Fatalf("expected grace period to remain active, suspended=%v err=%v", suspended, err)
	}
	if suspended, err := s.IsAccountSuspended(ctx, user.ID, failedAt.Add(14*24*time.Hour)); err != nil || !suspended {
		t.Fatalf("expected suspension at 14 days, suspended=%v err=%v", suspended, err)
	}
	if err := s.ClearAccountArrears(ctx, user.ID); err != nil {
		t.Fatalf("clear arrears: %v", err)
	}
	if suspended, err := s.IsAccountSuspended(ctx, user.ID, failedAt.Add(30*24*time.Hour)); err != nil || suspended {
		t.Fatalf("expected paid account to be restored, suspended=%v err=%v", suspended, err)
	}
}
