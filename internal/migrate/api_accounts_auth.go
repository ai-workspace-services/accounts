package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"
)

// Accounts owns token format, expiration and revocation. Migration never mints,
// refreshes or parses these credentials, and never caches authorization results.
func NewAccountsMigrationAuthorizer(origin string, allowLoopback bool) (MigrationAuthorizer, error) {
	u, err := migrationURL(origin, allowLoopback)
	if err != nil {
		return nil, err
	}
	endpoint := u.String() + "/api/admin/migration/authorize"
	client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 5 * time.Second}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	return func(ctx context.Context, token, scope string) (string, int) {
		if token == "" || len(token) > 8192 {
			return "", http.StatusUnauthorized
		}
		body, _ := json.Marshal(map[string]string{"scope": scope})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return "", http.StatusServiceUnavailable
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Content-Type", "application/json")
		response, err := client.Do(req)
		if err != nil {
			return "", http.StatusServiceUnavailable
		}
		defer response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
			return "", response.StatusCode
		}
		if response.StatusCode != http.StatusOK {
			return "", http.StatusServiceUnavailable
		}
		var result struct {
			OperatorID string `json:"operator_id"`
			Scope      string `json:"scope"`
		}
		decoder := json.NewDecoder(io.LimitReader(response.Body, 4096))
		if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || !migrationSHA.MatchString(result.OperatorID) || result.Scope != scope {
			return "", http.StatusServiceUnavailable
		}
		return result.OperatorID, 0
	}, nil
}
