package xrayconfig

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// ClientSource provides the list of active Xray clients to encode in the config.
type ClientSource interface {
	ListClients(ctx context.Context) ([]Client, error)
}

type commandRunner func(ctx context.Context, cmd []string) ([]byte, error)

// PeriodicOptions configures a PeriodicSyncer instance.
type PeriodicOptions struct {
	Logger          *slog.Logger
	Interval        time.Duration
	Source          ClientSource
	Generators      []Generator
	ValidateCommand []string
	RestartCommand  []string
	Runner          commandRunner
	OnSync          func(SyncResult)
}

// PeriodicSyncer periodically rebuilds the Xray configuration from the database.
type PeriodicSyncer struct {
	logger          *slog.Logger
	interval        time.Duration
	source          ClientSource
	generators      []Generator
	validateCommand []string
	restartCommand  []string
	runner          commandRunner
	onSync          func(SyncResult)
}

// SyncResult describes the outcome of a synchronization attempt.
type SyncResult struct {
	Clients     int
	Error       error
	CompletedAt time.Time
}

// NewPeriodicSyncer constructs a new PeriodicSyncer from the provided options.
func NewPeriodicSyncer(opts PeriodicOptions) (*PeriodicSyncer, error) {
	if opts.Source == nil {
		return nil, errors.New("client source is required")
	}
	if len(opts.Generators) == 0 {
		return nil, errors.New("at least one generator is required")
	}
	for i, g := range opts.Generators {
		if strings.TrimSpace(g.OutputPath) == "" {
			return nil, fmt.Errorf("generator %d output path is required", i)
		}
	}
	if opts.Interval <= 0 {
		return nil, errors.New("interval must be positive")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	runner := opts.Runner
	if runner == nil {
		runner = defaultCommandRunner
	}
	return &PeriodicSyncer{
		logger:          logger,
		interval:        opts.Interval,
		source:          opts.Source,
		generators:      append([]Generator(nil), opts.Generators...),
		validateCommand: append([]string(nil), opts.ValidateCommand...),
		restartCommand:  append([]string(nil), opts.RestartCommand...),
		runner:          runner,
		onSync:          opts.OnSync,
	}, nil
}

// Start launches the synchronization loop. The returned stop function cancels the
// loop and waits for it to exit, honouring the provided context for the wait.
func (s *PeriodicSyncer) Start(ctx context.Context) (func(context.Context) error, error) {
	if s == nil {
		return nil, errors.New("syncer is nil")
	}
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.run(runCtx)
	}()
	stop := func(waitCtx context.Context) error {
		cancel()
		if waitCtx == nil {
			waitCtx = context.Background()
		}
		select {
		case <-done:
			return nil
		case <-waitCtx.Done():
			return waitCtx.Err()
		}
	}
	return stop, nil
}

func (s *PeriodicSyncer) run(ctx context.Context) {
	if n, err := s.sync(ctx); err != nil {
		s.notify(SyncResult{Clients: n, Error: err, CompletedAt: time.Now().UTC()})
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			s.logger.Error("xray config sync failed", "err", err)
		}
		if ctx.Err() != nil {
			return
		}
	} else {
		s.logger.Info("xray config synchronized", "clients", n)
		s.notify(SyncResult{Clients: n, CompletedAt: time.Now().UTC()})
	}

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			n, err := s.sync(ctx)
			if err != nil {
				s.notify(SyncResult{Clients: n, Error: err, CompletedAt: time.Now().UTC()})
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return
				}
				s.logger.Error("xray config sync failed", "err", err)
				continue
			}
			s.logger.Info("xray config synchronized", "clients", n)
			s.notify(SyncResult{Clients: n, CompletedAt: time.Now().UTC()})
		}
	}
}

func (s *PeriodicSyncer) sync(ctx context.Context) (int, error) {
	clients, err := s.source.ListClients(ctx)
	if err != nil {
		return 0, fmt.Errorf("list clients: %w", err)
	}
	changed := false
	for _, g := range s.generators {
		written, err := g.GenerateIfChanged(clients)
		if err != nil {
			return 0, fmt.Errorf("generate config for %s: %w", g.OutputPath, err)
		}
		changed = changed || written
	}
	// A polling cycle with the same client list is intentionally a no-op. In
	// particular, it must not restart Xray merely because the controller was
	// polled again.
	if !changed {
		return len(clients), nil
	}
	if len(s.validateCommand) > 0 {
		if err := s.runCommand(ctx, s.validateCommand, "validate config"); err != nil {
			return 0, err
		}
	}
	if len(s.restartCommand) > 0 {
		if err := s.runCommand(ctx, s.restartCommand, "restart xray"); err != nil {
			return 0, err
		}
	}
	return len(clients), nil
}

func (s *PeriodicSyncer) notify(result SyncResult) {
	if s.onSync == nil {
		return
	}
	s.onSync(result)
}

func (s *PeriodicSyncer) runCommand(ctx context.Context, cmd []string, action string) error {
	output, err := s.runner(ctx, cmd)
	if err != nil {
		if len(output) > 0 {
			return fmt.Errorf("%s: %w: %s", action, err, strings.TrimSpace(string(output)))
		}
		return fmt.Errorf("%s: %w", action, err)
	}
	if len(output) > 0 {
		s.logger.Debug(action, "output", strings.TrimSpace(string(output)))
	}
	return nil
}

func defaultCommandRunner(ctx context.Context, cmd []string) ([]byte, error) {
	if len(cmd) == 0 {
		return nil, errors.New("command is empty")
	}
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	return c.CombinedOutput()
}
