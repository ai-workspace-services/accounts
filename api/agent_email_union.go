package api

import (
	"account/internal/agentproto"
	"account/internal/agentserver"
	"account/internal/store"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"strings"
)

const unionEmailPrefix = "node-email-union:"

func hasPublicUnionPolicy(groups []string) bool {
	for _, g := range groups {
		if g == "node-email-policy:"+agentproto.PublicEmailUnionPolicy {
			return true
		}
	}
	return false
}
func publicUnionEmails(groups []string) []string {
	emails := []string{}
	for _, g := range groups {
		if strings.HasPrefix(g, unionEmailPrefix) {
			emails = append(emails, strings.TrimPrefix(g, unionEmailPrefix))
		}
	}
	normalized, err := agentproto.NormalizeNodeEmails(emails)
	if err != nil {
		return nil
	}
	return normalized
}
func publicUnionEmailMatches(groups []string, user *store.User) bool {
	emails := publicUnionEmails(groups)
	return len(emails) > 0 && agentEmailAllowed(emails, user)
}

func (h *handler) resolveScopedAgent(c *gin.Context, credential agentserver.Identity, requested string) (agentserver.Identity, bool) {
	if credential.NodeID != "" {
		return h.resolveNodeBoundAgent(c, credential, requested)
	}
	identity, ok := h.resolveScopedAgentLegacy(c, credential, requested)
	if !ok {
		return identity, false
	}
	if e, ok := teamEnrollment(c); ok && e.Status != "approved" {
		if c.GetHeader("X-Agent-Allowed-Emails") != "" || c.GetHeader("X-Agent-Email-Policy") != "" {
			respondError(c, 400, "agent_email_policy_invalid", "team nodes do not support email extensions")
			return identity, false
		}
		return identity, true
	}
	scope, valid := agentResourceScope(identity.Groups)
	if !valid {
		respondError(c, 403, "agent_scope_invalid", "invalid node scope")
		return identity, false
	}
	raw := c.GetHeader("X-Agent-Allowed-Emails")
	policy := c.GetHeader("X-Agent-Email-Policy")
	if scope.ResourceClass == agentResourceCustom {
		return identity, true
	}
	if scope.ResourceClass == agentResourceTeam {
		if raw != "" || policy != "" {
			respondError(c, 400, "agent_email_policy_invalid", "team nodes do not support email extensions")
			return identity, false
		}
		return identity, true
	}
	groups := make([]string, 0, len(identity.Groups))
	for _, g := range identity.Groups {
		if !strings.HasPrefix(g, unionEmailPrefix) && !strings.HasPrefix(g, "node-email-policy:") {
			groups = append(groups, g)
		}
	}
	identity.Groups = groups
	if policy == "" && raw == "" {
		return identity, true
	}
	if !agentproto.PublicResourceClass(scope.ResourceClass) || policy != agentproto.PublicEmailUnionPolicy {
		respondError(c, 400, "agent_email_policy_invalid", "public email extension requires public-union-v1")
		return identity, false
	}
	var emails []string
	if err := json.Unmarshal([]byte(raw), &emails); err != nil {
		respondError(c, 400, "agent_email_policy_invalid", "email extension must be an array")
		return identity, false
	}
	manager := false
	cs, cv := agentResourceScope(credential.Groups)
	for _, g := range credential.Groups {
		if g == "agent:configure-public" || (g == "internal" && credential.ID == "*" && cv && cs.ResourceClass == agentResourceShared) {
			manager = true
		}
	}
	if !manager || (credential.ID != "*" && credential.ID != identity.ID) {
		respondError(c, 403, "agent_scope_mismatch", "email extension requires node configuration permission")
		return identity, false
	}

	emails, err := agentproto.NormalizeNodeEmails(emails)
	if err != nil {
		respondError(c, 400, "agent_email_policy_invalid", "invalid node email list")
		return identity, false
	}
	for _, email := range emails {
		identity.Groups = append(identity.Groups, unionEmailPrefix+email)
	}
	// Empty intent is also acknowledged, so removal has an explicit receipt.
	identity.Groups = append(identity.Groups, "node-email-policy:"+agentproto.PublicEmailUnionPolicy)
	return identity, true
}

// Policy bookkeeping is not an externally visible role or user group.
func publicAgentDisplayGroups(groups []string) []string {
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		if !strings.HasPrefix(g, unionEmailPrefix) && !strings.HasPrefix(g, "node-email-policy:") {
			out = append(out, g)
		}
	}
	return out
}
