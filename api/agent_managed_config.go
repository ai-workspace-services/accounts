package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const maxAgentManagedConfigBytes = 1 << 20

var managedConfigRevision = regexp.MustCompile("^[0-9a-f]{40}$")

// This version intentionally supports a subset of the Agent overlay fields.
type agentManagedSettings struct {
	Region         string `json:"region,omitempty"`
	Pool           string `json:"pool,omitempty"`
	StatusInterval string `json:"statusInterval,omitempty"`
	StatsEnabled   *bool  `json:"statsEnabled,omitempty"`
}

type agentManagedDocument struct {
	SchemaVersion int                        `json:"schemaVersion"`
	Revision      string                     `json:"revision"`
	Templates     map[string]json.RawMessage `json:"templates"`
	NodeOverrides map[string]json.RawMessage `json:"nodeOverrides,omitempty"`
}

type agentManagedSnapshot struct {
	SchemaVersion int                  `json:"schemaVersion"`
	Revision      string               `json:"revision"`
	NodeID        string               `json:"nodeId"`
	ResourceClass string               `json:"resourceClass"`
	Template      string               `json:"template"`
	Agent         agentManagedSettings `json:"agent"`
}

func WithAgentManagedConfigPath(path string) Option {
	return func(h *handler) { h.agentManagedConfigPath = strings.TrimSpace(path) }
}

func decodeManagedObject(data []byte, value any) error {
	data = bytes.TrimSpace(data)
	if len(data) == 0 || data[0] != '{' {
		return errors.New("expected managed configuration object")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("unexpected data after managed configuration")
	}
	return nil
}

func resolveAgentManagedConfig(data []byte, nodeID, resourceClass, template string) (agentManagedSnapshot, error) {
	var doc agentManagedDocument
	if err := decodeManagedObject(data, &doc); err != nil {
		return agentManagedSnapshot{}, err
	}
	if doc.SchemaVersion != 1 || !managedConfigRevision.MatchString(doc.Revision) || len(doc.Templates) != 4 {
		return agentManagedSnapshot{}, errors.New("invalid managed configuration manifest")
	}
	var selected agentManagedSettings
	for _, name := range []string{"free-shared", "plus", "pro", "custom"} {
		var settings agentManagedSettings
		if err := decodeManagedObject(doc.Templates[name], &settings); err != nil {
			return agentManagedSnapshot{}, err
		}
		if name == template {
			selected = settings
		}
	}
	if override, ok := doc.NodeOverrides[nodeID]; ok {
		if err := decodeManagedObject(override, &selected); err != nil {
			return agentManagedSnapshot{}, err
		}
	}
	if selected.StatusInterval != "" {
		interval, err := time.ParseDuration(selected.StatusInterval)
		if err != nil || interval <= 0 {
			return agentManagedSnapshot{}, errors.New("invalid status interval")
		}
	}
	return agentManagedSnapshot{doc.SchemaVersion, doc.Revision, nodeID, resourceClass, template, selected}, nil
}

func (h *handler) getAgentManagedConfig(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	if h.agentManagedConfigPath == "" {
		c.JSON(http.StatusNotFound, gin.H{"error": "agent_managed_config_disabled"})
		return
	}
	if h.agentRegistry == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "agent_registry_unavailable"})
		return
	}
	token := extractToken(c.GetHeader("Authorization"))
	if token == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "missing_token"})
		return
	}
	identity, ok := h.agentRegistry.Authenticate(token)
	if !ok || identity == nil {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid_token"})
		return
	}
	if identity.NodeID == "" || identity.ID == "*" {
		c.JSON(http.StatusForbidden, gin.H{"error": "agent_node_binding_required"})
		return
	}
	scope, valid := agentResourceScope(identity.Groups)
	template := scope.ResourceClass
	switch template {
	case agentResourceFree, agentResourceShared:
		template = "free-shared"
	case agentResourcePlus, agentResourcePro, agentResourceCustom:
	default:
		valid = false
	}
	if !valid {
		c.JSON(http.StatusForbidden, gin.H{"error": "agent_scope_invalid"})
		return
	}

	unavailable := gin.H{"error": "agent_managed_config_unavailable"}
	file, err := os.Open(h.agentManagedConfigPath)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, unavailable)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxAgentManagedConfigBytes+1))
	if err != nil || len(data) > maxAgentManagedConfigBytes {
		c.JSON(http.StatusServiceUnavailable, unavailable)
		return
	}
	snapshot, err := resolveAgentManagedConfig(data, identity.NodeID, scope.ResourceClass, template)
	if err != nil {
		c.JSON(http.StatusServiceUnavailable, unavailable)
		return
	}
	c.JSON(http.StatusOK, snapshot)
}
