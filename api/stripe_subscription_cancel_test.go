package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"account/internal/store"
)

func TestSubscriptionCancellationPermanentLocalReplay(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	u := &store.User{Name: "Cancel", Email: "cancel-review@example.invalid"}
	_ = st.CreateUser(ctx, u)
	sub := &store.Subscription{UserID: u.ID, Provider: "stripe", Kind: "subscription", ExternalID: "sub_cancel", Status: "active", PlanID: "PLUS"}
	_ = st.UpsertSubscription(ctx, sub)
	var posts atomic.Int32
	var scheduled atomic.Bool
	end := time.Now().Add(24 * time.Hour).Unix()
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/subscriptions/sub_cancel" {
			t.Error("wrong target", r.URL)
			http.Error(w, "wrong target", 400)
			return
		}
		if r.Method == "POST" {
			posts.Add(1)
			_ = r.ParseForm()
			if r.Header.Get("Idempotency-Key") != "subscription-cancel:"+sub.ID || r.Form.Get("cancel_at_period_end") != "true" {
				t.Error("unstable cancellation")
			}
			scheduled.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"sub_cancel","status":"active","metadata":{"user_id":%q},"cancel_at_period_end":%t,"current_period_start":%d,"current_period_end":%d}`, u.ID, scheduled.Load(), end-30*24*3600, end)
	}))
	defer provider.Close()
	client := newStripeClient(StripeConfig{SecretKey: "sk_test_cancel"})
	client.apiBaseURL = provider.URL
	client.httpClient = provider.Client()
	h := &handler{store: st, stripe: client}
	for range 3 {
		if err := h.cancelStripeSubscription(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	if posts.Load() != 1 || sub.Status != "canceling" {
		t.Fatal("repeated cancellation", posts.Load(), sub.Status)
	}
	s, _ := st.GetSubscriptionLifecycle(ctx, u.ID)
	if s.PendingPlanID != "FREE" || s.EffectiveAt.Unix() != end || s.LastChangeAt != nil || s.OperationID != "" {
		t.Fatal(s)
	}
}
