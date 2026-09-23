package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"account/internal/store"
)

func TestAdminDeleteUserArchivesAndAudits(t *testing.T) {
	h := newActiveHarness(t)
	if !h.target.Active {
		t.Fatal("test target should be active before archive")
	}

	req := httptest.NewRequest(http.MethodDelete,
		"/api/auth/admin/users/"+h.target.ID+"?reason=duplicate%20account", nil)
	req.Header.Set("Authorization", "Bearer "+h.adminToken)
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	archived, err := h.store.GetUserByID(context.Background(), h.target.ID)
	if err != nil {
		t.Fatalf("archived account must remain addressable: %v", err)
	}
	if archived.Active || archived.ArchivedAt == nil {
		t.Fatalf("expected inactive archived account, got active=%v archivedAt=%v", archived.Active, archived.ArchivedAt)
	}
	if _, err := h.store.GetUserByEmail(context.Background(), h.target.Email); err != nil {
		t.Fatalf("archiving must preserve email identity: %v", err)
	}

	entries, err := h.store.ListAuditLogs(context.Background(), store.AuditLogFilter{
		ActionPrefix: store.AuditActionUserArchive,
		TargetUUID:   h.target.ID,
	})
	if err != nil {
		t.Fatalf("list archive audit: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one archive audit entry, got %d", len(entries))
	}
	entry := entries[0]
	if entry.ActorUUID != "admin-1" {
		t.Fatalf("expected admin actor, got %q", entry.ActorUUID)
	}
	if entry.Details["reason"] != "duplicate account" {
		t.Fatalf("expected recorded reason, got %#v", entry.Details["reason"])
	}
}

func TestAdminDeleteUserRequiresReasonAndProtectsAdministrators(t *testing.T) {
	t.Run("reason required", func(t *testing.T) {
		h := newActiveHarness(t)
		req := httptest.NewRequest(http.MethodDelete, "/api/auth/admin/users/"+h.target.ID, nil)
		req.Header.Set("Authorization", "Bearer "+h.adminToken)
		rec := httptest.NewRecorder()
		h.router.ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("expected 400, got %d: %s", rec.Code, rec.Body.String())
		}
		user, err := h.store.GetUserByID(context.Background(), h.target.ID)
		if err != nil || user.ArchivedAt != nil || !user.Active {
			t.Fatalf("missing reason must leave account unchanged: user=%+v err=%v", user, err)
		}
	})

	for _, tc := range []struct {
		name       string
		selectUser func(*activeHarness) *store.User
	}{
		{name: "root", selectUser: func(h *activeHarness) *store.User { return h.root }},
		{name: "legacy admin", selectUser: func(h *activeHarness) *store.User { return h.target }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActiveHarness(t)
			target := tc.selectUser(h)
			if tc.name == "legacy admin" {
				target.Role = store.RoleAdmin
				if err := h.store.UpdateUser(context.Background(), target); err != nil {
					t.Fatalf("promote target to admin: %v", err)
				}
			}
			req := httptest.NewRequest(http.MethodDelete,
				"/api/auth/admin/users/"+target.ID+"?reason=retention%20check", nil)
			req.Header.Set("Authorization", "Bearer "+h.adminToken)
			rec := httptest.NewRecorder()
			h.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			stored, err := h.store.GetUserByID(context.Background(), target.ID)
			if err != nil || stored.ArchivedAt != nil {
				t.Fatalf("protected administrator must remain unarchived: user=%+v err=%v", stored, err)
			}
		})
	}
}

func TestAdminDeleteUserProtectsPaidAccounts(t *testing.T) {
	for _, tc := range []struct {
		name string
		seed func(*testing.T, store.Store, *store.User)
	}{
		{
			name: "active subscription",
			seed: func(t *testing.T, st store.Store, user *store.User) {
				t.Helper()
				if err := st.UpsertSubscription(context.Background(), &store.Subscription{
					UserID: user.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub-active", Status: "active",
				}); err != nil {
					t.Fatalf("seed paid subscription: %v", err)
				}
			},
		},
		{
			name: "paid quota segment",
			seed: func(t *testing.T, st store.Store, user *store.User) {
				t.Helper()
				user.Groups = []string{store.MonthlyPlusQuotaLimitGroup}
				if err := st.UpdateUser(context.Background(), user); err != nil {
					t.Fatalf("seed paid quota segment: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newActiveHarness(t)
			tc.seed(t, h.store, h.target)
			req := httptest.NewRequest(http.MethodDelete,
				"/api/auth/admin/users/"+h.target.ID+"?reason=paid%20account", nil)
			req.Header.Set("Authorization", "Bearer "+h.adminToken)
			rec := httptest.NewRecorder()
			h.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("expected 403, got %d: %s", rec.Code, rec.Body.String())
			}
			stored, err := h.store.GetUserByID(context.Background(), h.target.ID)
			if err != nil || stored.ArchivedAt != nil {
				t.Fatalf("paid account must remain unarchived: user=%+v err=%v", stored, err)
			}
		})
	}
}
