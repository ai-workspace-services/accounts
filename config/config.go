package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Log defines logging configuration for the account service.
type Log struct {
	// Level sets the minimum log level. Valid values are "debug", "info",
	// "warn", and "error".
	Level string `yaml:"level"`
}

// Config holds configuration for the account service.
type Config struct {
	Mode          string        `yaml:"mode"`
	Log           Log           `yaml:"log"`
	Server        Server        `yaml:"server"`
	Store         Store         `yaml:"store"`
	Session       Session       `yaml:"session"`
	Auth          Auth          `yaml:"auth"`
	SMTP          SMTP          `yaml:"smtp"`
	Xray          Xray          `yaml:"xray"`
	Agent         Agent         `yaml:"agent"`
	Agents        Agents        `yaml:"agents"`
	ReviewAccount ReviewAccount `yaml:"reviewAccount"`
}

// Server defines HTTP server configuration.
type Server struct {
	Addr           string        `yaml:"addr"`
	ReadTimeout    time.Duration `yaml:"readTimeout"`
	WriteTimeout   time.Duration `yaml:"writeTimeout"`
	TLS            TLS           `yaml:"tls"`
	PublicURL      string        `yaml:"publicUrl"`
	AllowedOrigins []string      `yaml:"allowedOrigins"`
}

// TLS describes TLS configuration for the server listener.
type TLS struct {
	Enabled      *bool  `yaml:"enabled"`
	CertFile     string `yaml:"certFile"`
	KeyFile      string `yaml:"keyFile"`
	CAFile       string `yaml:"caFile"`
	ClientCAFile string `yaml:"clientCAFile"`
	RedirectHTTP bool   `yaml:"redirectHttp"`
}

// IsEnabled reports whether TLS should be enabled for the server listener. When the
// configuration explicitly sets the Enabled field it is respected. Otherwise TLS is
// considered enabled only if both the certificate and key paths are non-empty.
func (t TLS) IsEnabled() bool {
	if t.Enabled != nil {
		return *t.Enabled
	}
	return strings.TrimSpace(t.CertFile) != "" && strings.TrimSpace(t.KeyFile) != ""
}

// Store defines persistence configuration for the account service.
type Store struct {
	Driver       string `yaml:"driver"`
	DSN          string `yaml:"dsn"`
	MaxOpenConns int    `yaml:"maxOpenConns"`
	MaxIdleConns int    `yaml:"maxIdleConns"`
}

// Session defines session management configuration.
type Session struct {
	TTL time.Duration `yaml:"ttl"`
}

// Auth defines authentication configuration.
type Auth struct {
	Enable bool  `yaml:"enable"`
	Token  Token `yaml:"token"`
	OAuth  OAuth `yaml:"oauth"`
}

// OAuth defines OAuth2 configuration for multiple providers.
type OAuth struct {
	RedirectURL string        `yaml:"redirectUrl"`
	FrontendURL string        `yaml:"frontendUrl"`
	GitHub      OAuthProvider `yaml:"github"`
	Google      OAuthProvider `yaml:"google"`
}

// OAuthProvider defines configuration for a single OAuth2 provider.
type OAuthProvider struct {
	ClientID     string `yaml:"clientId"`
	ClientSecret string `yaml:"clientSecret"`
	RedirectURL  string `yaml:"redirectUrl"`
}

// Token defines token authentication configuration.
type Token struct {
	PublicToken   string        `yaml:"publicToken"`
	RefreshSecret string        `yaml:"refreshSecret"`
	AccessSecret  string        `yaml:"accessSecret"`
	AccessExpiry  time.Duration `yaml:"accessExpiry"`
	RefreshExpiry time.Duration `yaml:"refreshExpiry"`
}

// SMTP defines outbound SMTP configuration used for transactional email.
type SMTP struct {
	Host     string        `yaml:"host"`
	Port     int           `yaml:"port"`
	Username string        `yaml:"username"`
	Password string        `yaml:"password"`
	From     string        `yaml:"from"`
	ReplyTo  string        `yaml:"replyTo"`
	Timeout  time.Duration `yaml:"timeout"`
	TLS      SMTPTLS       `yaml:"tls"`
}

// SMTPTLS describes TLS settings for SMTP connections. Mode supports "auto",
// "starttls", "implicit", and "none". The "auto" mode negotiates STARTTLS
// when the server advertises support and otherwise falls back to an unencrypted
// connection which is useful for local testing.
type SMTPTLS struct {
	Mode               string `yaml:"mode"`
	InsecureSkipVerify bool   `yaml:"insecureSkipVerify"`
}

// Xray groups configuration related to synchronizing the Xray proxy.
type Xray struct {
	Sync XraySync `yaml:"sync"`
}

// XraySync defines options for periodically updating the Xray configuration.
type XraySync struct {
	Enabled         bool          `yaml:"enabled"`
	Interval        time.Duration `yaml:"interval"`
	Domain          string        `yaml:"domain"`
	OutputPath      string        `yaml:"outputPath"`
	TemplatePath    string        `yaml:"templatePath"`
	ValidateCommand []string      `yaml:"validateCommand"`
	RestartCommand  []string      `yaml:"restartCommand"`
}

// Agent defines configuration for agent mode deployments.
type Agent struct {
	ID             string        `yaml:"id"`
	ControllerURL  string        `yaml:"controllerUrl"`
	APIToken       string        `yaml:"apiToken"`
	HTTPTimeout    time.Duration `yaml:"httpTimeout"`
	StatusInterval time.Duration `yaml:"statusInterval"`
	SyncInterval   time.Duration `yaml:"syncInterval"`
	TLS            AgentTLS      `yaml:"tls"`
}

// AgentTLS configures TLS behaviour for the agent HTTP client.
type AgentTLS struct {
	InsecureSkipVerify bool `yaml:"insecureSkipVerify"`
}

// Agents describes the controller-side agent configuration.
type Agents struct {
	Credentials []AgentCredential `yaml:"credentials"`
}

// ReviewAccount controls the built-in readonly review user intended for App
// Review and feature validation.
type ReviewAccount struct {
	Enabled     bool     `yaml:"enabled"`
	Email       string   `yaml:"email"`
	Name        string   `yaml:"name"`
	Password    string   `yaml:"password"`
	Groups      []string `yaml:"groups"`
	Permissions []string `yaml:"permissions"`
}

// AgentCredential represents a single agent identity authorised to call the
// controller API.
type AgentCredential struct {
	ID     string   `yaml:"id"`
	Name   string   `yaml:"name"`
	Token  string   `yaml:"token"`
	Groups []string `yaml:"groups"`
}

// Load reads the configuration file at the provided path. When path is empty,
// it uses the APP_ENV environment variable (default: dev) to load
// config/account-{APP_ENV}.yaml.
func Load(path string) (*Config, error) {
	p := path
	if p == "" {
		appEnv := strings.TrimSpace(os.Getenv("APP_ENV"))
		if appEnv == "" {
			appEnv = "dev"
		}
		// Try to find the file in the current directory or the 'config' subdirectory
		p = filepath.Join("config", "account-"+appEnv+".yaml")
		if _, err := os.Stat(p); os.IsNotExist(err) {
			p = "account-" + appEnv + ".yaml"
		}
	}

	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return &Config{}, nil
		}
		return nil, err
	}

	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}
