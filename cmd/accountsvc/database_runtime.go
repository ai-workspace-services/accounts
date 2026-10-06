package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"

	"account/api"
	"account/config"
)

// Standby deliberately constructs no business store, ORM, service or worker.
// Both source Cloud Run and target all-in-one use managed HTTPS ingress with
// plaintext application ports, so this probe server shares that boundary.
func runDatabaseStandby(ctx context.Context, cfg *config.Config) error {
	if cfg.Server.TLS.IsEnabled() {
		return errors.New("database standby requires managed HTTPS ingress termination")
	}
	addr := cfg.Server.Addr
	if addr == "" {
		addr = ":8080"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return errors.New("cannot bind database standby probe listener")
	}
	srv := &http.Server{Handler: cfg.DatabaseRuntime.Wrap(nil, api.RuntimeImageMetadata),
		ReadTimeout: cfg.Server.ReadTimeout, WriteTimeout: cfg.Server.WriteTimeout, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ln) }()
	select {
	case err = <-done:
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err = srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
			return err
		}
		err = <-done
	}
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
