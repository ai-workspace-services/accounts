package stripemock

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func get(t *testing.T, url, key string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestServesFixturesForTestKeysOnly(t *testing.T) {
	m := New(map[string]json.RawMessage{"subscriptions/sub_1": json.RawMessage(`{"id":"sub_1"}`)})
	srv := httptest.NewServer(m)
	defer srv.Close()
	if code, body := get(t, srv.URL+"/v1/subscriptions/sub_1", "sk_test_x"); code != 200 || !strings.Contains(body, "sub_1") {
		t.Fatalf("got %d %s", code, body)
	}
	if code, _ := get(t, srv.URL+"/v1/subscriptions/sub_1", "sk_live_x"); code != 401 {
		t.Fatalf("live key must be refused, got %d", code)
	}
	if code, _ := get(t, srv.URL+"/v1/invoices/in_missing", "rk_test_x"); code != 404 {
		t.Fatalf("missing fixture must 404, got %d", code)
	}
	if got := m.Requests(); len(got) != 2 || got[0] != "GET /v1/subscriptions/sub_1" {
		t.Fatalf("requests %v", got)
	}
}

func TestMethodSpecificFixture(t *testing.T) {
	m := New(nil)
	if err := m.Set("POST /v1/refunds", map[string]string{"id": "re_1"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(m)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/refunds", strings.NewReader("payment_intent=pi_1"))
	req.Header.Set("Authorization", "Bearer sk_test_x")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("refund fixture: %v %v", err, resp)
	}
}
