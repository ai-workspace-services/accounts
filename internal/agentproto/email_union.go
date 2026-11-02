package agentproto

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"sort"
	"strings"
)

const PublicEmailUnionPolicy = "public-union-v1"

func NormalizeNodeEmails(emails []string) ([]string, error) {
	if len(emails) > 256 {
		return nil, errors.New("node email list exceeds 256 entries")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(emails))
	for _, raw := range emails {
		email := strings.ToLower(strings.TrimSpace(raw))
		parsed, err := mail.ParseAddress(email)
		if err != nil || parsed.Address != email || strings.ContainsAny(email, "*?") {
			return nil, errors.New("invalid node email (wildcards and display names forbidden)")
		}
		if !seen[email] {
			seen[email] = true
			out = append(out, email)
		}
	}
	sort.Strings(out)
	return out, nil
}
func NodeEmailDigest(emails []string) string {
	sum := sha256.Sum256([]byte(strings.Join(emails, "\n")))
	return hex.EncodeToString(sum[:])
}
func PublicResourceClass(class string) bool {
	switch class {
	case "shared", "free", "plus", "pro":
		return true
	}
	return false
}
