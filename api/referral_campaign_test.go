package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"account/internal/store"
)

func TestReferralCampaignAdministrationAndPublicContract(t *testing.T) {
	h := newReferralContractHarness(t)
	_, member := h.user(t, "campaign-member", "Campaign member", "campaign-member@example.test", store.RoleUser)
	_, admin := h.user(t, "campaign-admin", "Campaign admin", "campaign-admin@example.test", store.RoleAdmin, permissionAdminBillingMoneyWrite)
	endpoint := "/api/auth/admin/referrals/campaign"
	for _, token := range []string{"", member} {
		rec := referralContractRequest(t, h.router, http.MethodPut, endpoint, token, []byte(`{"enabled":false,"reason":"close"}`))
		if rec.Code != http.StatusUnauthorized && rec.Code != http.StatusForbidden {
			t.Fatalf("unauthorized campaign mutation: %d", rec.Code)
		}
	}
	for _, body := range []string{
		`{"reason":"missing enabled"}`,
		`{"enabled":true,"reason":""}`,
		`{"enabled":true,"startsAt":"2030-02-01T00:00:00+08:00","endsAt":"2030-01-01T00:00:00+08:00","reason":"invalid bounds"}`,
		`{"enabled":true,"startsAt":"not-a-date","reason":"invalid date"}`,
	} {
		if rec := referralContractRequest(t, h.router, http.MethodPut, endpoint, admin, []byte(body)); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid configuration accepted: %d %s", rec.Code, rec.Body.String())
		}
	}
	configure := func(body string) store.ReferralCampaign {
		t.Helper()
		rec := referralContractRequest(t, h.router, http.MethodPut, endpoint, admin, []byte(body))
		if rec.Code != http.StatusOK {
			t.Fatalf("configure: %d %s", rec.Code, rec.Body.String())
		}
		var reply struct {
			Campaign store.ReferralCampaign `json:"campaign"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		return reply.Campaign
	}
	closed := configure(`{"enabled":false,"startsAt":null,"endsAt":null,"reason":"pause campaign","revision":999,"effectiveAt":"2000-01-01T00:00:00Z"}`)
	if closed.Enabled || closed.Revision != 2 || closed.EffectiveAt.Before(time.Now().Add(-time.Minute)) {
		t.Fatal("client rewrote server-owned campaign history")
	}
	public := referralContractRequest(t, h.router, http.MethodGet, "/api/referrals/campaign", "", nil)
	if public.Code != http.StatusOK || !strings.Contains(public.Body.String(), `"active":false`) || strings.Contains(public.Body.String(), "pause campaign") || strings.Contains(public.Body.String(), "actorId") || public.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("public campaign contract leaked audit data or stale state: %d %s", public.Code, public.Body.String())
	}
	opened := configure(`{"enabled":true,"startsAt":null,"endsAt":null,"reason":"open without fixed dates"}`)
	if !opened.Active(time.Now()) || opened.Revision != 3 || opened.StartsAt != nil || opened.EndsAt != nil {
		t.Fatal("unbounded activity did not open")
	}
	if rec := referralContractRequest(t, h.router, http.MethodGet, "/api/auth/referrals", member, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"active":true`) {
		t.Fatalf("overview does not use configured campaign: %d %s", rec.Code, rec.Body.String())
	}
	if rec := referralContractRequest(t, h.router, http.MethodGet, endpoint, admin, nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "open without fixed dates") {
		t.Fatal("administrator cannot read current audited configuration")
	}
}

func TestReferralCampaignOpenClosePreservesDefinedDates(t *testing.T) {
	h := newReferralContractHarness(t)
	_, admin := h.user(t, "campaign-switch-admin", "Campaign switch admin", "campaign-switch@example.test", store.RoleAdmin, permissionAdminBillingMoneyWrite)
	endpoint := "/api/auth/admin/referrals/campaign"
	rec := referralContractRequest(t, h.router, http.MethodPut, endpoint, admin, []byte(`{"enabled":false,"startsAt":"2030-01-01T00:00:00+08:00","endsAt":"2030-02-01T00:00:00+08:00","reason":"define future activity"}`))
	if rec.Code != http.StatusOK {
		t.Fatal("cannot define future activity")
	}
	for _, action := range []string{"open", "close", "open"} {
		rec = referralContractRequest(t, h.router, http.MethodPost, endpoint+"/"+action, admin, []byte(`{"reason":"operational switch"}`))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s status=%d", action, rec.Code)
		}
		var reply struct {
			Campaign store.ReferralCampaign `json:"campaign"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &reply); err != nil {
			t.Fatal(err)
		}
		if reply.Campaign.StartsAt == nil || reply.Campaign.EndsAt == nil || reply.Campaign.StartsAt.UTC().Format(time.RFC3339) != "2029-12-31T16:00:00Z" || reply.Campaign.EndsAt.UTC().Format(time.RFC3339) != "2030-01-31T16:00:00Z" || reply.Campaign.Enabled != (action == "open") {
			t.Fatal("switch changed the defined schedule")
		}
	}
}
