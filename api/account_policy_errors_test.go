package api

import (
	"account/internal/store"
	"context"
	"errors"
	"github.com/gin-gonic/gin"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type policyFailureStore struct {
	store.Store
	failure error
}

func (s policyFailureStore) GetLatestAccountPolicySnapshot(context.Context, string) (*store.AccountPolicySnapshot, error) {
	return nil, s.failure
}

func TestAccountPolicyDistinguishesMissingSnapshotFromStoreFailure(t *testing.T) {
	for _, tc := range []struct {
		failure error
		status  int
		code    string
	}{
		{store.ErrUserNotFound, 404, "policy_not_found"},
		{errors.New("database unavailable"), 500, "account_policy_unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			st := store.NewMemoryStore()
			u := seedActiveUser(t, st, "Policy", "policy@example.invalid", "policy-proxy")
			if err := st.CreateSession(context.Background(), "policy-session", u.ID, time.Now().Add(time.Hour)); err != nil {
				t.Fatal(err)
			}
			h := &handler{store: policyFailureStore{Store: st, failure: tc.failure}}
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest("GET", "/api/account/policy", nil)
			c.Request.Header.Set("Authorization", "Bearer policy-session")
			h.accountPolicy(c)
			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.code) {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
		})
	}
}
