package main

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"account/internal/migrate"
	"github.com/spf13/cobra"
)

var migrationEnvName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func migrationEnv(name string) (string, error) {
	if !migrationEnvName.MatchString(name) {
		return "", errors.New("valid environment variable reference required")
	}
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return "", errors.New("referenced environment variable is empty")
	}
	return value, nil
}
func migrationKeyEnv(name string) ([]byte, error) {
	value, err := migrationEnv(name)
	if err != nil {
		return nil, err
	}
	key, err := base64.StdEncoding.DecodeString(value)
	if err != nil || len(key) != 32 {
		return nil, errors.New("key environment must contain base64 of exactly 32 random bytes")
	}
	return key, nil
}

func migrationCredential(ctx context.Context, env, kv string) (string, error) {
	if (env == "") == (kv == "") {
		return "", errors.New("provide exactly one token-env or auth-kv reference for each required endpoint")
	}
	var value string
	var err error
	if env != "" {
		value, err = migrationEnv(env)
	} else {
		if strings.HasPrefix(kv, "-") || strings.ContainsAny(kv, "\r\n\x00") || len(kv) > 512 {
			return "", errors.New("invalid Vault KV reference")
		}
		// Vault's authenticated process environment supplies its short-lived Vault
		// identity; token data is captured in memory and never echoed or written.
		command := exec.CommandContext(ctx, "vault", "kv", "get", "-field=token", kv)
		var output []byte
		output, err = command.Output()
		if err != nil {
			return "", errors.New("migration credential lookup failed")
		}
		value = strings.TrimSpace(string(output))
	}
	if err != nil {
		return "", err
	}
	if value == "" || len(value) > 8192 || strings.ContainsAny(value, "\r\n") {
		return "", errors.New("invalid migration credential")
	}
	return value, nil
}

func newMigrationAPICmd() *cobra.Command {
	var options migrate.MigrationClientOptions
	var sourceEnv, targetEnv, sourceKV, targetKV string
	cmd := &cobra.Command{Use: "transfer-api", Short: "Preview identity and billing transfer through opt-in migration APIs", Args: cobra.NoArgs, SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		if options.Apply && options.Status {
			return errors.New("--apply and --status are mutually exclusive")
		}
		if options.TransferOnly && (options.Apply || options.Status) {
			return errors.New("--transfer-only cannot be combined with --apply or --status")
		}
		if options.Apply && (options.PlanID == "" || options.PlanSHA256 == "") {
			return errors.New("apply requires exact --plan-id and --plan-sha256")
		}
		if !options.Apply && options.PlanSHA256 != "" {
			return errors.New("--plan-sha256 is only valid with --apply")
		}
		if options.TargetURL == "" || (!options.Apply && !options.Status && options.SourceURL == "") {
			return errors.New("source-url and target-url required for preview; target-url required for apply/status")
		}
		var err error
		options.TargetToken, err = migrationCredential(cmd.Context(), targetEnv, targetKV)
		if err != nil {
			return err
		}
		if !options.Apply && !options.Status {
			options.SourceToken, err = migrationCredential(cmd.Context(), sourceEnv, sourceKV)
			if err != nil {
				return err
			}
		} else if sourceEnv != "" && sourceKV != "" {
			return errors.New("source token-env and auth-kv are mutually exclusive")
		}
		result, err := migrate.RunMigrationClient(cmd.Context(), options)
		if err != nil {
			return err
		}
		if err = json.NewEncoder(cmd.OutOrStdout()).Encode(result); err != nil {
			return err
		}
		if result.State == "conflict" {
			return errors.New("migration preview has conflicts; apply is unavailable")
		}
		return nil
	}}
	flags := cmd.Flags()
	flags.StringVar(&options.SourceURL, "source-url", "", "HTTPS source Accounts migration origin")
	flags.StringVar(&options.TargetURL, "target-url", "", "HTTPS target Accounts migration origin")
	flags.StringVar(&sourceEnv, "source-token-env", "", "Name of environment variable holding short-lived source migration token")
	flags.StringVar(&targetEnv, "target-token-env", "", "Name of environment variable holding short-lived target migration token")
	flags.StringVar(&sourceKV, "source-auth-kv", "", "Vault KV reference with a token field")
	flags.StringVar(&targetKV, "target-auth-kv", "", "Vault KV reference with a token field")
	flags.BoolVar(&options.Apply, "apply", false, "Apply only the exact persisted preview")
	flags.BoolVar(&options.TransferOnly, "transfer-only", false, "Resume and verify encrypted chunks without business preview or apply")
	flags.BoolVar(&options.Status, "status", false, "Read the persisted plan or committed receipt")
	flags.StringVar(&options.PlanID, "plan-id", "", "Exact reviewed plan UUID; optional caller-generated idempotency UUID for preview")
	flags.StringVar(&options.PlanSHA256, "plan-sha256", "", "Exact reviewed preview SHA-256; required to apply")
	flags.StringVar(&options.SnapshotID, "snapshot-id", "", "Optional caller-generated snapshot UUID for recovery")
	flags.BoolVar(&options.AllowLoopbackHTTP, "allow-loopback-http", false, "Explicit local-only HTTP development transport")
	return cmd
}

func newMigrationTokenCmd() *cobra.Command {
	var keyEnv, audience, subject string
	var binding migrate.MigrationAuthorization
	var scopes []string
	var lifetime time.Duration
	cmd := &cobra.Command{Use: "migration-token", Short: "Mint a short-lived migration-only token from a dedicated signing key", Args: cobra.NoArgs, SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		key, err := migrationKeyEnv(keyEnv)
		if err != nil {
			return err
		}
		token, err := migrate.MintMigrationToken(key, audience, subject, scopes, lifetime, binding)
		if err != nil {
			return err
		}
		_, err = cmd.OutOrStdout().Write([]byte(token + "\n"))
		return err
	}}
	f := cmd.Flags()
	f.StringVar(&keyEnv, "auth-key-env", "", "Base64 dedicated migration signing-key environment reference")
	f.StringVar(&audience, "audience", "", "Exact listener audience")
	f.StringVar(&binding.SourceAudience, "source-audience", "", "Exact approved source migration listener audience")
	f.StringVar(&binding.TargetAudience, "target-audience", "", "Exact approved target migration listener audience")
	f.StringVar(&binding.JobID, "job-id", "", "Exact approved migration job UUID")
	f.StringVar(&subject, "subject", "", "Stable migration operator identifier (use an opaque UUID)")
	f.StringSliceVar(&scopes, "scopes", nil, "Allowed source.read,target.plan,target.apply scopes; target.plan covers preview/status")
	f.DurationVar(&lifetime, "ttl", 10*time.Minute, "Token lifetime, maximum 15 minutes")
	return cmd
}

func newMigrationServeCmd() *cobra.Command {
	var options migrate.MigrationServerOptions
	var dsnEnv, authEnv, stageEnv, address, cert, key, backupReceipt string
	var peerURLs, peerCIDRs []string
	var local bool
	cmd := &cobra.Command{Use: "serve", Short: "Run a separate opt-in migration API listener", Args: cobra.NoArgs, SilenceUsage: true, RunE: func(cmd *cobra.Command, args []string) error {
		var err error
		if backupReceipt != "" {
			if options.Role != "target" {
				return errors.New("backup receipt applies to target role only")
			}
			options.BackupGate, err = migrate.MigrationBackupFileGate(backupReceipt)
			if err != nil {
				return err
			}
		}
		options.AuthKey, err = migrationKeyEnv(authEnv)
		if err != nil {
			return err
		}
		options.StagingKey, err = migrationKeyEnv(stageEnv)
		if err != nil {
			return err
		}
		dsn, err := migrationEnv(dsnEnv)
		if err != nil {
			return err
		}
		if (cert == "") != (key == "") {
			return errors.New("both TLS certificate and key required")
		}
		if cert == "" {
			host, _, err := net.SplitHostPort(address)
			ip, e := netip.ParseAddr(host)
			if err != nil || !local || e != nil || !ip.IsLoopback() {
				return errors.New("TLS required; explicit loopback HTTP is available for local proxy/development only")
			}
		}
		options.Peers.URLs = peerURLs
		options.Peers.AllowLoopbackHTTP = local
		for _, cidr := range peerCIDRs {
			prefix, err := netip.ParsePrefix(cidr)
			if err != nil {
				return errors.New("invalid peer CIDR")
			}
			options.Peers.Networks = append(options.Peers.Networks, prefix)
		}
		if options.Role == "target" && (len(peerURLs) == 0 || len(peerCIDRs) == 0) {
			return errors.New("target requires explicit allowed source URLs and peer CIDRs")
		}
		db, err := sql.Open("pgx", dsn)
		if err != nil {
			return errors.New("migration database unavailable")
		}
		defer db.Close()
		db.SetMaxOpenConns(6)
		options.DB = db
		ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
		err = db.PingContext(ctx)
		cancel()
		if err != nil {
			return errors.New("migration database unavailable")
		}
		handler, err := migrate.NewMigrationHandler(options)
		if err != nil {
			return err
		}
		server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 6 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
		ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		go func() {
			<-ctx.Done()
			shutdown, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			_ = server.Shutdown(shutdown)
		}()
		if cert != "" {
			err = server.ListenAndServeTLS(cert, key)
		} else {
			err = server.ListenAndServe()
		}
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		if err != nil {
			return errors.New("migration listener failed")
		}
		return nil
	}}
	f := cmd.Flags()
	f.StringVar(&options.Role, "role", "", "Explicit source or target listener")
	f.StringVar(&options.Audience, "audience", "", "Unique migration listener audience")
	f.StringVar(&options.SourceAudience, "source-audience", "", "Approved source migration listener audience")
	f.StringVar(&options.TargetAudience, "target-audience", "", "Approved target migration listener audience")
	f.StringVar(&options.JobID, "job-id", "", "Approved migration job UUID; shared by this exact source/target pair")
	f.StringVar(&dsnEnv, "dsn-env", "", "Database DSN environment reference")
	f.StringVar(&authEnv, "auth-key-env", "", "Base64 32-byte dedicated migration signing-key environment reference")
	f.StringVar(&stageEnv, "staging-key-env", "", "Base64 distinct 32-byte staging encryption-key environment reference")
	f.StringVar(&options.StagingDirectory, "staging-dir", "", "Absolute persistent private mode-0700 directory")
	f.StringVar(&backupReceipt, "backup-receipt-file", "", "Protected migration-owner verified backup/restore receipt; enables guarded apply")
	f.BoolVar(&options.InitializeReceipts, "initialize-receipts", false, "Explicit migration-owner schema initialization for apply (never implicit in preview)")
	f.StringVar(&options.ReceiptOwnerRole, "receipt-owner-role", "", "Exact migration-owner DB role required by --initialize-receipts")
	f.DurationVar(&options.Lifetime, "object-ttl", time.Hour, "Snapshot/plan retention validity, at most 24 hours")
	f.StringVar(&address, "listen", "127.0.0.1:9097", "Separate listener bind address")
	f.StringVar(&cert, "tls-cert", "", "TLS certificate file")
	f.StringVar(&key, "tls-key", "", "TLS private-key file")
	f.StringSliceVar(&peerURLs, "allowed-source-url", nil, "Exact allowed source origins")
	f.StringSliceVar(&peerCIDRs, "allowed-peer-cidr", nil, "Explicit allowed source network CIDRs (DNS pinned)")
	f.BoolVar(&local, "allow-loopback-http", false, "Explicit loopback HTTP for local proxy/development")
	return cmd
}
