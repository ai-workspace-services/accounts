package xrayconfig

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

const (
	// DefaultFlow is applied to VLESS clients when no explicit flow is
	// provided. It matches the tlsSettings baked into the template.
	DefaultFlow = "xtls-rprx-vision"
)

// Client represents an entry under inbounds[0].settings.clients[] in the Xray
// config.
type Client struct {
	ID    string
	Email string
	Flow  string
}

// Generator updates the Xray configuration file based on a template and a set of
// active clients.
type Generator struct {
	// Definition provides the base Xray configuration that will be mutated to
	// include the provided clients. When nil, DefaultDefinition is used.
	Definition Definition

	// OutputPath is the destination path for the generated configuration
	// (typically /usr/local/etc/xray/config.json).
	OutputPath string

	// FileMode controls the permissions for the generated file. When zero it
	// defaults to 0644.
	FileMode fs.FileMode

	// Domain is the hostname used to interpolate templates (e.g. for cert paths).
	Domain string
}

// Generate writes a new Xray configuration with the provided clients. The base
// template is loaded on every invocation to ensure updates remain additive and
// idempotent even when multiple callers trigger regeneration.
func (g Generator) Generate(clients []Client) error {
	_, err := g.GenerateIfChanged(clients)
	return err
}

// GenerateIfChanged writes only when the rendered configuration differs from
// the active file. The agent polls regularly, so avoiding identical writes
// also avoids unnecessary Xray reloads/restarts between access changes.
func (g Generator) GenerateIfChanged(clients []Client) (bool, error) {
	if strings.TrimSpace(g.OutputPath) == "" {
		return false, errors.New("output path is required")
	}

	buf, err := g.Render(clients)
	if err != nil {
		return false, err
	}
	if current, err := os.ReadFile(g.OutputPath); err == nil && bytes.Equal(current, buf) {
		return false, nil
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("read existing config: %w", err)
	}

	mode := g.FileMode
	if mode == 0 {
		mode = 0o644
	}
	if err := atomicWriteFile(g.OutputPath, buf, mode); err != nil {
		return false, fmt.Errorf("write config: %w", err)
	}

	return true, nil
}

// Render returns the rendered configuration JSON without writing it to disk.
// The returned buffer always ends with a newline.
func (g Generator) Render(clients []Client) ([]byte, error) {
	definition := g.Definition
	if definition == nil {
		definition = DefaultDefinition()
	}

	root, err := g.renderTemplate(definition)
	if err != nil {
		return nil, fmt.Errorf("render template: %w", err)
	}

	if err := replaceClients(root, clients); err != nil {
		return nil, err
	}

	buf, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	buf = append(buf, '\n')
	return buf, nil
}

func (g Generator) renderTemplate(definition Definition) (map[string]interface{}, error) {
	jsonDef, ok := definition.(JSONDefinition)
	if !ok {
		return definition.Base()
	}

	tmpl, err := template.New("xray").Parse(string(jsonDef.Raw))
	if err != nil {
		return nil, fmt.Errorf("parse template: %w", err)
	}

	data := struct {
		Domain string
	}{
		Domain: g.Domain,
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("execute template: %w", err)
	}

	var root map[string]interface{}
	if err := json.Unmarshal([]byte(buf.String()), &root); err != nil {
		return nil, fmt.Errorf("unmarshal rendered template: %w", err)
	}
	return root, nil
}

func replaceClients(root map[string]interface{}, clients []Client) error {
	inboundsValue, ok := root["inbounds"]
	if !ok {
		return errors.New("template missing inbounds array")
	}

	inboundsSlice, ok := inboundsValue.([]interface{})
	if !ok {
		return fmt.Errorf("template inbounds has unexpected type %T", inboundsValue)
	}
	if len(inboundsSlice) == 0 {
		return errors.New("template missing inbound entry")
	}

	inbound, ok := inboundsSlice[0].(map[string]interface{})
	if !ok {
		return fmt.Errorf("template inbound 0 has unexpected type %T", inboundsSlice[0])
	}

	// Determine if we should include flow based on network
	includeFlow := true
	if streamSettings, ok := inbound["streamSettings"].(map[string]interface{}); ok {
		if network, _ := streamSettings["network"].(string); network == "xhttp" {
			includeFlow = false
		}
	}

	clientObjects := make([]interface{}, 0, len(clients))
	for idx, client := range clients {
		id := strings.TrimSpace(client.ID)
		if id == "" {
			return fmt.Errorf("client %d missing id", idx)
		}
		entry := map[string]interface{}{
			"id": id,
		}
		if email := strings.TrimSpace(client.Email); email != "" {
			entry["email"] = email
		}

		if includeFlow {
			flow := strings.TrimSpace(client.Flow)
			if flow == "" {
				flow = DefaultFlow
			}
			entry["flow"] = flow
		}
		clientObjects = append(clientObjects, entry)
	}

	settingsValue, ok := inbound["settings"]
	if !ok {
		return errors.New("template inbound missing settings object")
	}

	settingsMap, ok := settingsValue.(map[string]interface{})
	if !ok {
		return fmt.Errorf("template inbound settings has unexpected type %T", settingsValue)
	}

	clientsValue, ok := settingsMap["clients"]
	if !ok {
		return errors.New("template inbound missing clients array")
	}

	if _, ok := clientsValue.([]interface{}); !ok {
		return fmt.Errorf("template inbound clients has unexpected type %T", clientsValue)
	}

	// Always replace the clients array so the config reflects the exact
	// state from the database.
	settingsMap["clients"] = clientObjects
	inbound["settings"] = settingsMap
	inboundsSlice[0] = inbound
	root["inbounds"] = inboundsSlice
	return nil
}

func atomicWriteFile(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create directory %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".xray-config-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}()

	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("chmod temp file: %w", err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}
