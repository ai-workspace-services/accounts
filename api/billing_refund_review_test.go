package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"account/internal/store"
	"github.com/gin-gonic/gin"
)

func TestRefundApplicationWindowUsesPaymentNotSubscription(t *testing.T) {
	paid := time.Date(2026, 10, 20, 1, 2, 3, 0, time.UTC)
	for _, tc := range []struct {
		at   time.Time
		want bool
	}{
		{paid, true}, {paid.Add(72 * time.Hour), true}, {paid.Add(72*time.Hour + time.Nanosecond), false}, {paid.Add(-time.Nanosecond), false}, {time.Time{}, false},
	} {
		if got := refundWithinWindow(paid, tc.at); got != tc.want {
			t.Fatalf("at %s got %v", tc.at, got)
		}
	}
	if refundWithinWindow(time.Time{}, paid) {
		t.Fatal("missing payment date accepted")
	}
}

func TestRefundApplicationNoAutomaticProviderExecution(t *testing.T) {
	h, user, sub, _, sandbox := refundOperationHarness(t)
	ctx := context.Background()
	if err := h.store.CreateSession(ctx, "review-session", user.ID, time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	router := gin.New()
	router.POST("/refund", h.refundSubscription)
	router.GET("/refund", h.subscriptionRefundStatus)
	request := func(method string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "/refund", strings.NewReader(`{"externalId":"sub_V3Sandbox"}`))
		r.Header.Set("Authorization", "Bearer review-session")
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		return w
	}
	first := request("POST")
	if first.Code != 202 || !strings.Contains(first.Body.String(), `"status":"needs_investigation"`) || !strings.Contains(first.Body.String(), `"refunded":false`) {
		t.Fatal(first.Code, first.Body.String())
	}
	op, err := h.store.GetFinanceOperation(ctx, refundReviewKey(user.ID))
	if err != nil {
		t.Fatal(err)
	}
	var intent refundReviewRequest
	if err := json.Unmarshal(op.Request, &intent); err != nil {
		t.Fatal(err)
	}
	if intent.SubscriptionID != sub.ID || intent.RequestedAt.IsZero() {
		t.Fatal("missing pinned identity/time")
	}
	// Later submission and an upgraded/replaced local row cannot change the
	// original request timestamp or use the user route to execute a refund.
	sub.PlanID = "PRO-ANNUAL-V2"
	_ = h.store.UpsertSubscription(ctx, sub)
	for range 3 {
		if w := request("POST"); w.Code != 202 || w.Body.String() != first.Body.String() {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	if w := request("GET"); w.Code != 202 || w.Body.String() != first.Body.String() {
		t.Fatal(w.Code, w.Body.String())
	}
	if sandbox.posts != 0 || sandbox.deletes != 0 {
		t.Fatal("application executed provider mutation")
	}
	facts, _ := h.store.ListFinanceRefunds(ctx, user.ID, 0)
	if len(facts) != 0 {
		t.Fatal("application recorded a refund")
	}
	user.MFAEnabled = false
	_ = h.store.UpdateUser(ctx, user)
	if w := request("POST"); w.Code != http.StatusForbidden {
		t.Fatal("MFA bypass", w.Code)
	}
	if op.OperationType != "refund_review" || op.Status != store.FinanceOperationInProgress {
		t.Fatal("review mixed with executable refund")
	}
}
