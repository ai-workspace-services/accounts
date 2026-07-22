package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

const defaultAccountServer = "https://accounts.svc.plus"
const overlayTransportType = "vless-tls"
const overlayTransportSecurity = "tls"

type stateFile struct {
	Server              string    `json:"server"`
	Token               string    `json:"token"`
	UserID              string    `json:"user_id,omitempty"`
	DeviceID            string    `json:"device_id,omitempty"`
	NetworkID           string    `json:"network_id,omitempty"`
	WireGuardPrivateKey string    `json:"wireguard_private_key,omitempty"`
	WireGuardPublicKey  string    `json:"wireguard_public_key,omitempty"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type loginResponse struct {
	Token       string         `json:"token"`
	AccessToken string         `json:"access_token"`
	User        map[string]any `json:"user"`
	Error       string         `json:"error"`
	Message     string         `json:"message"`
}

type overlayConfig struct {
	SchemaVersion int    `json:"schema_version"`
	Revision      string `json:"revision"`
	Digest        string `json:"digest"`
	Network       struct {
		ID string `json:"id"`
	} `json:"network"`
	Device struct {
		ID string `json:"id"`
	} `json:"device"`
	WireGuard struct {
		Interface           string   `json:"interface"`
		Address             string   `json:"address"`
		MTU                 int      `json:"mtu"`
		DNS                 []string `json:"dns"`
		PeerPublicKey       string   `json:"peer_public_key"`
		PeerAllowedIPs      []string `json:"peer_allowed_ips"`
		PeerEndpoint        string   `json:"peer_endpoint"`
		PersistentKeepalive int      `json:"persistent_keepalive"`
		GatewayWireGuardIP  string   `json:"gateway_wireguard_ip"`
		LocalProxyEndpoint  string   `json:"local_proxy_endpoint"`
	} `json:"wireguard"`
	Transport struct {
		Runtime        string `json:"runtime"`
		Type           string `json:"type"`
		Security       string `json:"security"`
		Server         string `json:"server"`
		Port           int    `json:"port"`
		UUID           string `json:"uuid"`
		Path           string `json:"path"`
		Mode           string `json:"mode"`
		Flow           string `json:"flow"`
		PacketEncoding string `json:"packet_encoding"`
		LocalPort      int    `json:"local_port"`
	} `json:"transport"`
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "overlayctl",
		Short: "Login, sync, and render the Cloud Neutral WireGuard-over-VLESS overlay config",
	}
	root.AddCommand(newLoginCmd())
	root.AddCommand(newRegisterDeviceCmd())
	root.AddCommand(newSyncConfigCmd())
	root.AddCommand(newAckConfigCmd())
	root.AddCommand(newRenderCmd())
	root.AddCommand(newExportPlaybooksClientCmd())
	root.AddCommand(newApplyPlaybooksClientCmd())
	root.AddCommand(newPreflightCmd())
	root.AddCommand(newUpCmd())
	root.AddCommand(newDownCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newCheckConnectivityCmd())
	return root
}

func newLoginCmd() *cobra.Command {
	var server, email, password string
	cmd := &cobra.Command{
		Use:   "login",
		Short: "Authenticate against accounts.svc.plus and persist a session token",
		RunE: func(cmd *cobra.Command, args []string) error {
			server = normalizeServer(server)
			if strings.TrimSpace(email) == "" || strings.TrimSpace(password) == "" {
				return errors.New("--email and --password are required")
			}
			body := map[string]string{"email": email, "password": password}
			var resp loginResponse
			if err := doJSON(http.MethodPost, server+"/api/auth/login", "", body, &resp); err != nil {
				return err
			}
			token := strings.TrimSpace(resp.Token)
			if token == "" {
				token = strings.TrimSpace(resp.AccessToken)
			}
			if token == "" {
				return fmt.Errorf("login did not return a token: %s", resp.Error)
			}
			state, _ := loadState()
			state.Server = server
			state.Token = token
			state.UpdatedAt = time.Now().UTC()
			if userID, ok := resp.User["uuid"].(string); ok {
				state.UserID = userID
			}
			if err := saveState(state); err != nil {
				return err
			}
			fmt.Printf("Logged in to %s\n", server)
			return nil
		},
	}
	cmd.Flags().StringVar(&server, "server", defaultAccountServer, "account service base URL")
	cmd.Flags().StringVar(&email, "email", "", "account email")
	cmd.Flags().StringVar(&password, "password", "", "account password")
	return cmd
}

func newRegisterDeviceCmd() *cobra.Command {
	var deviceID, name, platform, hostname, publicKey, privateKey string
	var generateKey bool
	cmd := &cobra.Command{
		Use:   "register-device",
		Short: "Register the local WireGuard device and assign an overlay address",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := requireState()
			if err != nil {
				return err
			}
			if deviceID == "" {
				deviceID = defaultDeviceID()
			}
			if platform == "" {
				platform = runtime.GOOS
			}
			if hostname == "" {
				hostname, _ = os.Hostname()
			}
			if generateKey {
				privateKey, publicKey, err = generateWireGuardKeypair()
				if err != nil {
					return err
				}
			}
			if strings.TrimSpace(publicKey) == "" {
				return errors.New("--public-key is required unless --generate-key succeeds")
			}
			if err := validateWireGuardKey("wireguard public key", publicKey); err != nil {
				return err
			}
			payload := map[string]string{
				"device_id":            deviceID,
				"name":                 firstNonEmpty(name, deviceID),
				"platform":             platform,
				"hostname":             hostname,
				"wireguard_public_key": publicKey,
			}
			var resp struct {
				Device struct {
					ID        string `json:"id"`
					NetworkID string `json:"network_id"`
				} `json:"device"`
			}
			if err := doJSON(http.MethodPost, state.Server+"/api/overlay/devices/register", state.Token, payload, &resp); err != nil {
				return err
			}
			state.DeviceID = resp.Device.ID
			state.NetworkID = resp.Device.NetworkID
			state.WireGuardPublicKey = publicKey
			if strings.TrimSpace(privateKey) != "" {
				state.WireGuardPrivateKey = privateKey
			}
			state.UpdatedAt = time.Now().UTC()
			if err := saveState(state); err != nil {
				return err
			}
			fmt.Printf("Registered device %s in network %s\n", state.DeviceID, state.NetworkID)
			return nil
		},
	}
	cmd.Flags().StringVar(&deviceID, "device-id", "", "stable local device id")
	cmd.Flags().StringVar(&name, "name", "", "display name")
	cmd.Flags().StringVar(&platform, "platform", "", "platform label")
	cmd.Flags().StringVar(&hostname, "hostname", "", "device hostname")
	cmd.Flags().StringVar(&publicKey, "public-key", "", "WireGuard public key")
	cmd.Flags().StringVar(&privateKey, "private-key", "", "WireGuard private key to store locally")
	cmd.Flags().BoolVar(&generateKey, "generate-key", false, "generate a WireGuard keypair using wg")
	return cmd
}

func newSyncConfigCmd() *cobra.Command {
	var output, nodeID string
	cmd := &cobra.Command{
		Use:   "sync-config",
		Short: "Download the overlay config contract for the registered device",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := requireState()
			if err != nil {
				return err
			}
			if strings.TrimSpace(state.DeviceID) == "" {
				return errors.New("device is not registered; run register-device first")
			}
			values := url.Values{}
			values.Set("device_id", state.DeviceID)
			if strings.TrimSpace(state.NetworkID) != "" {
				values.Set("network_id", state.NetworkID)
			}
			if strings.TrimSpace(nodeID) != "" {
				values.Set("node_id", nodeID)
			}
			url := state.Server + "/api/overlay/config?" + values.Encode()
			var config overlayConfig
			if err := doJSON(http.MethodGet, url, state.Token, nil, &config); err != nil {
				return err
			}
			if output == "" {
				output = defaultConfigPath()
			}
			if err := writeJSONFile(output, config); err != nil {
				return err
			}
			fmt.Printf("Synced overlay config revision %s to %s\n", config.Revision, output)
			return nil
		},
	}
	cmd.Flags().StringVar(&output, "output", "", "output config JSON path")
	cmd.Flags().StringVar(&nodeID, "node-id", "", "preferred overlay gateway node id")
	return cmd
}

func newAckConfigCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "ack-config",
		Short: "Acknowledge the currently applied overlay config revision",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := requireState()
			if err != nil {
				return err
			}
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			payload, err := buildConfigAckPayload(state, config, time.Now().UTC())
			if err != nil {
				return err
			}
			var resp map[string]any
			if err := doJSON(http.MethodPost, state.Server+"/api/overlay/config/ack", state.Token, payload, &resp); err != nil {
				return err
			}
			fmt.Printf("Acked overlay config revision %s for device %s\n", payload["revision"], payload["device_id"])
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	return cmd
}

func newRenderCmd() *cobra.Command {
	var configPath, outputDir string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Render WireGuard and Xray runtime files from the synced config contract",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := requireState()
			if err != nil {
				return err
			}
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			if outputDir == "" {
				outputDir = defaultStateDir()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			if err := validateOverlayRuntimeConfig(config); err != nil {
				return err
			}
			if strings.TrimSpace(state.WireGuardPrivateKey) == "" {
				return errors.New("wireguard private key is missing; register with --generate-key or --private-key")
			}
			if err := validateWireGuardKey("wireguard private key", state.WireGuardPrivateKey); err != nil {
				return err
			}
			if err := os.MkdirAll(outputDir, 0o700); err != nil {
				return err
			}
			wgPath := filepath.Join(outputDir, config.WireGuard.Interface+".conf")
			xrayPath := filepath.Join(outputDir, "xray-overlay.json")
			if err := os.WriteFile(wgPath, []byte(renderWireGuardConfig(config, state.WireGuardPrivateKey)), 0o600); err != nil {
				return err
			}
			if err := os.WriteFile(xrayPath, []byte(renderXrayConfig(config)), 0o600); err != nil {
				return err
			}
			fmt.Printf("Rendered %s\nRendered %s\n", wgPath, xrayPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&outputDir, "output-dir", "", "runtime output directory")
	return cmd
}

func newExportPlaybooksClientCmd() *cobra.Command {
	var configPath, output string
	var attachTo []string
	cmd := &cobra.Command{
		Use:   "export-playbooks-client",
		Short: "Export the registered device as an Ansible client peer fragment",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := requireState()
			if err != nil {
				return err
			}
			if strings.TrimSpace(state.DeviceID) == "" || strings.TrimSpace(state.WireGuardPublicKey) == "" {
				return errors.New("device is not registered; run register-device first")
			}
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			rendered, err := renderPlaybooksClientFragment(state, config, attachTo)
			if err != nil {
				return err
			}
			if output == "" || output == "-" {
				fmt.Print(rendered)
				return nil
			}
			if err := os.MkdirAll(filepath.Dir(output), 0o700); err != nil {
				return err
			}
			if err := os.WriteFile(output, []byte(rendered), 0o600); err != nil {
				return err
			}
			fmt.Printf("Exported playbooks client fragment to %s\n", output)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&output, "output", "-", "output YAML path, or - for stdout")
	cmd.Flags().StringSliceVar(&attachTo, "attach-to", nil, "limit the server-side WireGuard peer to these inventory hostnames")
	return cmd
}

func newApplyPlaybooksClientCmd() *cobra.Command {
	var configPath, groupVarsPath string
	var attachTo []string
	cmd := &cobra.Command{
		Use:   "apply-playbooks-client",
		Short: "Merge the registered device into a playbooks group_vars file",
		RunE: func(cmd *cobra.Command, args []string) error {
			state, err := requireState()
			if err != nil {
				return err
			}
			if strings.TrimSpace(state.DeviceID) == "" || strings.TrimSpace(state.WireGuardPublicKey) == "" {
				return errors.New("device is not registered; run register-device first")
			}
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			if groupVarsPath == "" {
				return errors.New("--group-vars is required")
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			client, err := playbooksClientFromState(state, config)
			if err != nil {
				return err
			}
			client.AttachTo = normalizeAttachTo(attachTo)
			if err := mergePlaybooksClientFile(groupVarsPath, client); err != nil {
				return err
			}
			fmt.Printf("Merged overlay client %s into %s\n", client.ID, groupVarsPath)
			return nil
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&groupVarsPath, "group-vars", "", "playbooks group_vars YAML path")
	cmd.Flags().StringSliceVar(&attachTo, "attach-to", nil, "limit the server-side WireGuard peer to these inventory hostnames")
	return cmd
}

func newPreflightCmd() *cobra.Command {
	var configPath, runtimeDir string
	cmd := &cobra.Command{
		Use:   "preflight",
		Short: "Check local WireGuard/Xray runtime prerequisites and rendered config files",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			if runtimeDir == "" {
				runtimeDir = defaultStateDir()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			return runPreflight(config, runtimeDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&runtimeDir, "runtime-dir", "", "directory containing rendered runtime files")
	return cmd
}

func newUpCmd() *cobra.Command {
	var configPath, runtimeDir string
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Start local Xray transport and bring up the WireGuard interface",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			if runtimeDir == "" {
				runtimeDir = defaultStateDir()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			return runOverlayUp(config, runtimeDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&runtimeDir, "runtime-dir", "", "directory containing rendered runtime files")
	return cmd
}

func newDownCmd() *cobra.Command {
	var configPath, runtimeDir string
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Tear down the local WireGuard interface and Xray transport",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			if runtimeDir == "" {
				runtimeDir = defaultStateDir()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			return runOverlayDown(config, runtimeDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&runtimeDir, "runtime-dir", "", "directory containing rendered runtime files")
	return cmd
}

func newStatusCmd() *cobra.Command {
	var configPath, runtimeDir string
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show local overlay runtime status",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			if runtimeDir == "" {
				runtimeDir = defaultStateDir()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			return runOverlayStatus(config, runtimeDir)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().StringVar(&runtimeDir, "runtime-dir", "", "directory containing rendered runtime files")
	return cmd
}

func newCheckConnectivityCmd() *cobra.Command {
	var configPath, httpURL, bearer string
	var pingCount int
	var skipPing, skipHTTP bool
	cmd := &cobra.Command{
		Use:   "check-connectivity",
		Short: "Check WireGuard gateway reachability after overlayctl up",
		RunE: func(cmd *cobra.Command, args []string) error {
			if configPath == "" {
				configPath = defaultConfigPath()
			}
			var config overlayConfig
			if err := readJSONFile(configPath, &config); err != nil {
				return err
			}
			if httpURL == "" {
				httpURL = defaultConnectivityURL(config)
			}
			return checkConnectivity(config, connectivityOptions{
				PingCount: pingCount,
				HTTPURL:   httpURL,
				Bearer:    bearer,
				SkipPing:  skipPing,
				SkipHTTP:  skipHTTP,
			})
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "synced overlay config path")
	cmd.Flags().IntVar(&pingCount, "ping-count", 3, "number of ICMP echo requests")
	cmd.Flags().StringVar(&httpURL, "http-url", "", "HTTP URL to check over the overlay")
	cmd.Flags().StringVar(&bearer, "bearer", "", "optional bearer token for the HTTP check")
	cmd.Flags().BoolVar(&skipPing, "skip-ping", false, "skip ICMP ping check")
	cmd.Flags().BoolVar(&skipHTTP, "skip-http", false, "skip HTTP check")
	return cmd
}

type playbooksClient struct {
	ID        string   `yaml:"id"`
	WGIP      string   `yaml:"wg_ip"`
	PublicKey string   `yaml:"public_key"`
	AttachTo  []string `yaml:"attach_to,omitempty"`
}

type playbooksGroupVars struct {
	Clients []playbooksClient `yaml:"xworkmate_bridge_distributed_vpn_clients"`
}

func runPreflight(config overlayConfig, runtimeDir string) error {
	if err := validateOverlayRuntimeConfig(config); err != nil {
		return err
	}
	wgPath, err := exec.LookPath("wg")
	if err != nil {
		return errors.New("wg binary is required")
	}
	wgQuickPath, err := exec.LookPath("wg-quick")
	if err != nil {
		return errors.New("wg-quick binary is required")
	}
	xrayPath, err := exec.LookPath("xray")
	if err != nil {
		return errors.New("xray binary is required")
	}

	wgConfigPath := filepath.Join(runtimeDir, config.WireGuard.Interface+".conf")
	xrayConfigPath := filepath.Join(runtimeDir, "xray-overlay.json")
	if _, err := os.Stat(wgConfigPath); err != nil {
		return fmt.Errorf("wireguard config is not rendered: %w", err)
	}
	if _, err := os.Stat(xrayConfigPath); err != nil {
		return fmt.Errorf("xray config is not rendered: %w", err)
	}

	testCmd := exec.Command(xrayPath, "run", "-test", "-config", xrayConfigPath)
	output, err := testCmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("xray config test failed: %w\n%s", err, strings.TrimSpace(string(output)))
	}

	fmt.Printf("wg: %s\n", wgPath)
	fmt.Printf("wg-quick: %s\n", wgQuickPath)
	fmt.Printf("xray: %s\n", xrayPath)
	fmt.Printf("wireguard config: %s\n", wgConfigPath)
	fmt.Printf("xray config: %s\n", xrayConfigPath)
	fmt.Println("xray config: OK")
	return nil
}

func doJSON(method, url, token string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		body = bytes.NewReader(buf)
	}
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if strings.TrimSpace(token) != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s failed: %s: %s", method, url, resp.Status, strings.TrimSpace(string(data)))
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func buildConfigAckPayload(state stateFile, config overlayConfig, appliedAt time.Time) (map[string]string, error) {
	deviceID := firstNonEmpty(config.Device.ID, state.DeviceID)
	networkID := firstNonEmpty(config.Network.ID, state.NetworkID)
	if strings.TrimSpace(deviceID) == "" {
		return nil, errors.New("device id is missing")
	}
	if strings.TrimSpace(config.Revision) == "" {
		return nil, errors.New("config revision is missing")
	}
	return map[string]string{
		"device_id":  deviceID,
		"network_id": networkID,
		"revision":   config.Revision,
		"digest":     config.Digest,
		"applied_at": appliedAt.UTC().Format(time.RFC3339),
	}, nil
}

func generateWireGuardKeypair() (string, string, error) {
	gen := exec.Command("wg", "genkey")
	privateBytes, err := gen.Output()
	if err != nil {
		return "", "", fmt.Errorf("wg genkey: %w", err)
	}
	privateKey := strings.TrimSpace(string(privateBytes))
	pub := exec.Command("wg", "pubkey")
	pub.Stdin = strings.NewReader(privateKey + "\n")
	publicBytes, err := pub.Output()
	if err != nil {
		return "", "", fmt.Errorf("wg pubkey: %w", err)
	}
	return privateKey, strings.TrimSpace(string(publicBytes)), nil
}

func renderWireGuardConfig(config overlayConfig, privateKey string) string {
	return fmt.Sprintf(`[Interface]
PrivateKey = %s
Address = %s
MTU = %d
DNS = %s

[Peer]
PublicKey = %s
AllowedIPs = %s
Endpoint = %s
PersistentKeepalive = %d
`, privateKey,
		config.WireGuard.Address,
		config.WireGuard.MTU,
		strings.Join(config.WireGuard.DNS, ", "),
		config.WireGuard.PeerPublicKey,
		strings.Join(config.WireGuard.PeerAllowedIPs, ", "),
		config.WireGuard.PeerEndpoint,
		config.WireGuard.PersistentKeepalive,
	)
}

func validateOverlayRuntimeConfig(config overlayConfig) error {
	var missing []string
	if strings.TrimSpace(config.WireGuard.Interface) == "" {
		missing = append(missing, "wireguard.interface")
	}
	if strings.TrimSpace(config.WireGuard.Address) == "" {
		missing = append(missing, "wireguard.address")
	}
	if strings.TrimSpace(config.WireGuard.PeerPublicKey) == "" {
		missing = append(missing, "wireguard.peer_public_key")
	}
	if len(config.WireGuard.PeerAllowedIPs) == 0 {
		missing = append(missing, "wireguard.peer_allowed_ips")
	}
	if strings.TrimSpace(config.WireGuard.PeerEndpoint) == "" {
		missing = append(missing, "wireguard.peer_endpoint")
	}
	if strings.TrimSpace(config.WireGuard.GatewayWireGuardIP) == "" {
		missing = append(missing, "wireguard.gateway_wireguard_ip")
	}
	if strings.TrimSpace(config.Transport.Server) == "" {
		missing = append(missing, "transport.server")
	}
	if config.Transport.Port <= 0 {
		missing = append(missing, "transport.port")
	}
	if strings.TrimSpace(config.Transport.Type) == "" {
		missing = append(missing, "transport.type")
	}
	if strings.TrimSpace(config.Transport.UUID) == "" {
		missing = append(missing, "transport.uuid")
	}
	if strings.TrimSpace(config.Transport.Security) == "" {
		missing = append(missing, "transport.security")
	}
	if len(missing) > 0 {
		return fmt.Errorf("overlay config is incomplete: %s", strings.Join(missing, ", "))
	}
	if err := validateWireGuardKey("wireguard.peer_public_key", config.WireGuard.PeerPublicKey); err != nil {
		return err
	}
	if _, err := uuid.Parse(strings.TrimSpace(config.Transport.UUID)); err != nil {
		return errors.New("transport.uuid must be a valid UUID")
	}
	if !isValidPort(config.Transport.Port) {
		return errors.New("transport.port must be between 1 and 65535")
	}
	if strings.TrimSpace(config.Transport.Type) != overlayTransportType {
		return fmt.Errorf("transport.type must be %s", overlayTransportType)
	}
	if strings.TrimSpace(config.Transport.Security) != overlayTransportSecurity {
		return fmt.Errorf("transport.security must be %s", overlayTransportSecurity)
	}
	if config.Transport.LocalPort != 0 && !isValidPort(config.Transport.LocalPort) {
		return errors.New("transport.local_port must be between 1 and 65535")
	}
	if config.WireGuard.MTU < 0 {
		return errors.New("overlay config is invalid: wireguard.mtu must be zero or positive")
	}
	if config.WireGuard.PersistentKeepalive < 0 {
		return errors.New("overlay config is invalid: wireguard.persistent_keepalive must be zero or positive")
	}
	return nil
}

func isValidPort(port int) bool {
	return port >= 1 && port <= 65535
}

func validateWireGuardKey(name, value string) error {
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(value))
	if err != nil || len(decoded) != 32 {
		return fmt.Errorf("%s must be a 32-byte base64 WireGuard key", name)
	}
	return nil
}

func renderPlaybooksClientFragment(state stateFile, config overlayConfig, attachTo []string) (string, error) {
	client, err := playbooksClientFromState(state, config)
	if err != nil {
		return "", err
	}
	client.AttachTo = normalizeAttachTo(attachTo)
	payload := playbooksGroupVars{Clients: []playbooksClient{client}}
	buf, err := yaml.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(buf), nil
}

func playbooksClientFromState(state stateFile, config overlayConfig) (playbooksClient, error) {
	client := playbooksClient{
		ID:        firstNonEmpty(config.Device.ID, state.DeviceID),
		WGIP:      stripCIDRSuffix(config.WireGuard.Address),
		PublicKey: strings.TrimSpace(state.WireGuardPublicKey),
	}
	if strings.TrimSpace(client.ID) == "" {
		return client, errors.New("device id is missing")
	}
	if client.PublicKey == "" {
		return client, errors.New("wireguard public key is missing")
	}
	if client.WGIP == "" {
		return client, errors.New("wireguard address is missing from synced config")
	}
	return client, nil
}

func mergePlaybooksClientFile(path string, client playbooksClient) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	var vars map[string]any
	if len(bytes.TrimSpace(data)) == 0 {
		vars = make(map[string]any)
	} else if err := yaml.Unmarshal(data, &vars); err != nil {
		return err
	}
	if vars == nil {
		vars = make(map[string]any)
	}

	clients := decodePlaybooksClients(vars["xworkmate_bridge_distributed_vpn_clients"])
	replaced := false
	for i := range clients {
		if clients[i].ID == client.ID {
			if len(client.AttachTo) == 0 {
				client.AttachTo = clients[i].AttachTo
			}
			clients[i] = client
			replaced = true
			break
		}
	}
	if !replaced {
		clients = append(clients, client)
	}
	vars["xworkmate_bridge_distributed_vpn_clients"] = clients

	buf, err := yaml.Marshal(vars)
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o600)
}

func decodePlaybooksClients(value any) []playbooksClient {
	buf, err := yaml.Marshal(value)
	if err != nil {
		return nil
	}
	var clients []playbooksClient
	if err := yaml.Unmarshal(buf, &clients); err != nil {
		return nil
	}
	return clients
}

func normalizeAttachTo(values []string) []string {
	seen := make(map[string]bool)
	normalized := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			item := strings.TrimSpace(part)
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			normalized = append(normalized, item)
		}
	}
	return normalized
}

func renderXrayConfig(config overlayConfig) string {
	localPort := config.Transport.LocalPort
	if localPort == 0 {
		localPort = 51830
	}
	user := map[string]any{
		"id":         config.Transport.UUID,
		"encryption": "none",
	}
	if flow := strings.TrimSpace(config.Transport.Flow); flow != "" {
		user["flow"] = flow
	}
	packetEncoding := strings.TrimSpace(config.Transport.PacketEncoding)
	if packetEncoding == "" {
		packetEncoding = "xudp"
	}
	user["packetEncoding"] = packetEncoding

	xray := map[string]any{
		"log": map[string]any{"loglevel": "info"},
		"inbounds": []map[string]any{{
			"listen":   "127.0.0.1",
			"port":     localPort,
			"protocol": "dokodemo-door",
			"settings": map[string]any{
				"address": config.WireGuard.GatewayWireGuardIP,
				"port":    51820,
				"network": "udp",
			},
			"tag": "wireguard-udp-in",
		}},
		"outbounds": []map[string]any{{
			"protocol": "vless",
			"settings": map[string]any{
				"vnext": []map[string]any{{
					"address": config.Transport.Server,
					"port":    config.Transport.Port,
					"users":   []map[string]any{user},
				}},
			},
			"streamSettings": map[string]any{
				"network":  "tcp",
				"security": config.Transport.Security,
				"tlsSettings": map[string]any{
					"serverName":    config.Transport.Server,
					"allowInsecure": false,
					"fingerprint":   "chrome",
				},
			},
			"tag": "proxy",
		}},
	}
	buf, _ := json.MarshalIndent(xray, "", "  ")
	return string(buf) + "\n"
}

func requireState() (stateFile, error) {
	state, err := loadState()
	if err != nil {
		return state, err
	}
	if strings.TrimSpace(state.Server) == "" || strings.TrimSpace(state.Token) == "" {
		return state, errors.New("not logged in; run overlayctl login first")
	}
	return state, nil
}

func runOverlayUp(config overlayConfig, runtimeDir string) error {
	if err := runPreflight(config, runtimeDir); err != nil {
		return err
	}

	xrayPath, _ := exec.LookPath("xray")
	wgQuickPath, _ := exec.LookPath("wg-quick")
	xrayConfigPath := filepath.Join(runtimeDir, "xray-overlay.json")
	wgConfigPath := filepath.Join(runtimeDir, config.WireGuard.Interface+".conf")
	pidPath := overlayXrayPIDPath(runtimeDir)

	if err := clearStalePIDFile(pidPath); err != nil {
		return err
	}

	xrayCmd := exec.Command(xrayPath, "run", "-config", xrayConfigPath)
	xrayLogPath := filepath.Join(runtimeDir, "xray-overlay.log")
	xrayLog, err := os.OpenFile(xrayLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer xrayLog.Close()
	xrayCmd.Stdout = xrayLog
	xrayCmd.Stderr = xrayLog
	if err := xrayCmd.Start(); err != nil {
		return fmt.Errorf("start xray: %w", err)
	}
	if err := os.WriteFile(pidPath, []byte(fmt.Sprintf("%d\n", xrayCmd.Process.Pid)), 0o600); err != nil {
		_ = xrayCmd.Process.Kill()
		return err
	}
	if err := waitForStartedProcess(xrayCmd, xrayLogPath); err != nil {
		_ = os.Remove(pidPath)
		return err
	}

	if output, err := exec.Command(wgQuickPath, "up", wgConfigPath).CombinedOutput(); err != nil {
		_ = xrayCmd.Process.Kill()
		_ = os.Remove(pidPath)
		return fmt.Errorf("wg-quick up failed: %w\n%s", err, strings.TrimSpace(string(output)))
	}

	fmt.Printf("xray started with pid %d\n", xrayCmd.Process.Pid)
	fmt.Printf("wireguard interface up: %s\n", config.WireGuard.Interface)
	return nil
}

func runOverlayDown(config overlayConfig, runtimeDir string) error {
	wgQuickPath, err := exec.LookPath("wg-quick")
	if err == nil {
		wgConfigPath := filepath.Join(runtimeDir, config.WireGuard.Interface+".conf")
		if output, downErr := exec.Command(wgQuickPath, "down", wgConfigPath).CombinedOutput(); downErr != nil {
			fmt.Fprintf(os.Stderr, "wg-quick down failed: %v\n%s\n", downErr, strings.TrimSpace(string(output)))
		}
	}

	pidPath := overlayXrayPIDPath(runtimeDir)
	pid, err := readPIDFile(pidPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			fmt.Println("xray pid file not found")
			return nil
		}
		_ = os.Remove(pidPath)
		return err
	}
	if !processRunning(pid) {
		_ = os.Remove(pidPath)
		fmt.Println("removed stale xray pid file")
		return nil
	}
	process, err := os.FindProcess(pid)
	if err == nil {
		_ = process.Signal(os.Interrupt)
		time.Sleep(500 * time.Millisecond)
		_ = process.Kill()
	}
	_ = os.Remove(pidPath)
	fmt.Println("overlay down")
	return nil
}

func runOverlayStatus(config overlayConfig, runtimeDir string) error {
	wgPath, _ := exec.LookPath("wg")
	pid, pidErr := readPIDFile(overlayXrayPIDPath(runtimeDir))
	if pidErr == nil {
		if processRunning(pid) {
			fmt.Printf("xray pid: %d\n", pid)
		} else {
			fmt.Printf("xray pid: stale (%d)\n", pid)
		}
	} else {
		fmt.Println("xray pid: not running")
	}

	if wgPath == "" {
		fmt.Println("wireguard: wg binary not found")
		return nil
	}
	output, err := exec.Command(wgPath, "show", config.WireGuard.Interface).CombinedOutput()
	if err != nil {
		fmt.Printf("wireguard interface %s: unavailable\n", config.WireGuard.Interface)
		return nil
	}
	fmt.Print(string(output))
	return nil
}

type connectivityOptions struct {
	PingCount int
	HTTPURL   string
	Bearer    string
	SkipPing  bool
	SkipHTTP  bool
}

func checkConnectivity(config overlayConfig, opts connectivityOptions) error {
	if opts.PingCount <= 0 {
		opts.PingCount = 3
	}
	gatewayIP := strings.TrimSpace(config.WireGuard.GatewayWireGuardIP)
	if gatewayIP == "" && !opts.SkipPing {
		return errors.New("gateway wireguard ip is missing")
	}

	if !opts.SkipPing {
		pingPath, err := exec.LookPath("ping")
		if err != nil {
			return errors.New("ping binary is required")
		}
		pingCmd := exec.Command(pingPath, "-c", fmt.Sprintf("%d", opts.PingCount), gatewayIP)
		if output, err := pingCmd.CombinedOutput(); err != nil {
			return fmt.Errorf("ping %s failed: %w\n%s", gatewayIP, err, strings.TrimSpace(string(output)))
		}
		fmt.Printf("ping %s: OK\n", gatewayIP)
	}

	if !opts.SkipHTTP {
		targetURL := strings.TrimSpace(opts.HTTPURL)
		if targetURL == "" {
			return errors.New("http url is required")
		}
		req, err := http.NewRequest(http.MethodGet, targetURL, nil)
		if err != nil {
			return err
		}
		if strings.TrimSpace(opts.Bearer) != "" {
			req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(opts.Bearer))
		}
		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("http check %s failed: %w", targetURL, err)
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
			return fmt.Errorf("http check %s failed: %s: %s", targetURL, resp.Status, strings.TrimSpace(string(body)))
		}
		fmt.Printf("http %s: %s\n", targetURL, resp.Status)
	}
	return nil
}

func defaultConnectivityURL(config overlayConfig) string {
	gatewayIP := strings.TrimSpace(config.WireGuard.GatewayWireGuardIP)
	if gatewayIP == "" {
		return ""
	}
	return fmt.Sprintf("http://%s:8787/api/ping", gatewayIP)
}

func overlayXrayPIDPath(runtimeDir string) string {
	return filepath.Join(runtimeDir, "xray-overlay.pid")
}

func clearStalePIDFile(path string) error {
	pid, err := readPIDFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return fmt.Errorf("remove invalid xray pid file %s: %w", path, removeErr)
		}
		return nil
	}
	if processRunning(pid) {
		return fmt.Errorf("xray pid file already exists for running process %d: %s", pid, path)
	}
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("remove stale xray pid file %s: %w", path, err)
	}
	return nil
}

func waitForStartedProcess(cmd *exec.Cmd, logPath string) error {
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	select {
	case err := <-done:
		if err == nil {
			return fmt.Errorf("xray exited immediately; inspect %s", logPath)
		}
		return fmt.Errorf("xray exited immediately: %w; inspect %s", err, logPath)
	case <-time.After(750 * time.Millisecond):
		return nil
	}
}

func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return exec.Command("kill", "-0", fmt.Sprintf("%d", pid)).Run() == nil
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(data)), "%d", &pid); err != nil {
		return 0, err
	}
	if pid <= 0 {
		return 0, errors.New("invalid pid")
	}
	return pid, nil
}

func loadState() (stateFile, error) {
	var state stateFile
	path := defaultStatePath()
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return stateFile{Server: defaultAccountServer}, nil
		}
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, err
	}
	state.Server = normalizeServer(state.Server)
	return state, nil
}

func saveState(state stateFile) error {
	if err := os.MkdirAll(defaultStateDir(), 0o700); err != nil {
		return err
	}
	state.Server = normalizeServer(state.Server)
	return writeJSONFile(defaultStatePath(), state)
}

func readJSONFile(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func writeJSONFile(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	buf = append(buf, '\n')
	return os.WriteFile(path, buf, 0o600)
}

func defaultStateDir() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".xoverlay"
	}
	return filepath.Join(home, ".xoverlay")
}

func defaultStatePath() string {
	return filepath.Join(defaultStateDir(), "session.json")
}

func defaultConfigPath() string {
	return filepath.Join(defaultStateDir(), "overlay-config.json")
}

func defaultDeviceID() string {
	hostname, _ := os.Hostname()
	hostname = strings.TrimSpace(strings.ToLower(hostname))
	if hostname == "" {
		hostname = runtime.GOOS
	}
	return strings.ReplaceAll(hostname, " ", "-")
}

func normalizeServer(server string) string {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" {
		return defaultAccountServer
	}
	return server
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func stripCIDRSuffix(address string) string {
	address = strings.TrimSpace(address)
	if address == "" {
		return ""
	}
	if idx := strings.Index(address, "/"); idx >= 0 {
		return strings.TrimSpace(address[:idx])
	}
	return address
}
