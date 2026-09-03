package xrayconfig

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type staticSource struct {
	clients []Client
	err     error
}

const minimalTemplate = `{"inbounds":[{"settings":{"clients":[]}}]}`

func (s staticSource) ListClients(context.Context) ([]Client, error) {
	if s.err != nil {
		return nil, s.err
	}
	return append([]Client(nil), s.clients...), nil
}

func TestNewPeriodicSyncerValidation(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.json")
	tests := []struct {
		name string
		opts PeriodicOptions
	}{
		{
			name: "missing source",
			opts: PeriodicOptions{
				Interval:   time.Minute,
				Generators: []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
			},
		},
		{
			name: "missing output",
			opts: PeriodicOptions{
				Interval:   time.Minute,
				Source:     staticSource{},
				Generators: []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}}},
			},
		},
		{
			name: "non-positive interval",
			opts: PeriodicOptions{
				Interval:   0,
				Source:     staticSource{},
				Generators: []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewPeriodicSyncer(tt.opts); err == nil {
				t.Fatalf("expected error")
			}
		})
	}
}

func TestPeriodicSyncerSyncSuccess(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.json")
	opts := PeriodicOptions{
		Interval:        time.Minute,
		Source:          staticSource{clients: []Client{{ID: "uuid-a", Email: "a@example"}, {ID: "uuid-b"}}},
		Generators:      []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
		ValidateCommand: []string{"echo", "validate"},
		RestartCommand:  []string{"echo", "restart"},
	}

	var commands [][]string
	opts.Runner = func(_ context.Context, cmd []string) ([]byte, error) {
		commands = append(commands, append([]string(nil), cmd...))
		return []byte(strings.Join(cmd, " ")), nil
	}

	syncer, err := NewPeriodicSyncer(opts)
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}

	n, err := syncer.sync(context.Background())
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 clients, got %d", n)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if !strings.Contains(string(data), "uuid-a") || !strings.Contains(string(data), "uuid-b") {
		t.Fatalf("output missing client ids: %s", string(data))
	}
	if got, want := len(commands), 2; got != want {
		t.Fatalf("expected %d commands, got %d", want, got)
	}
	if commands[0][0] != "echo" || commands[1][0] != "echo" {
		t.Fatalf("unexpected commands: %+v", commands)
	}
}

func TestPeriodicSyncerSkipsRestartWhenClientConfigIsUnchanged(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.json")
	var restarts int
	syncer, err := NewPeriodicSyncer(PeriodicOptions{
		Interval:       time.Minute,
		Source:         staticSource{clients: []Client{{ID: "uuid-a"}}},
		Generators:     []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
		RestartCommand: []string{"xray", "restart"},
		Runner: func(_ context.Context, cmd []string) ([]byte, error) {
			if len(cmd) > 1 && cmd[1] == "restart" {
				restarts++
			}
			return nil, nil
		},
	})
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	if _, err := syncer.sync(context.Background()); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := syncer.sync(context.Background()); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if restarts != 1 {
		t.Fatalf("expected one restart for an unchanged config, got %d", restarts)
	}
}

func TestPeriodicSyncerSyncError(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.json")
	opts := PeriodicOptions{
		Interval:   time.Minute,
		Source:     staticSource{err: errors.New("boom")},
		Generators: []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
	}
	syncer, err := NewPeriodicSyncer(opts)
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	if _, err := syncer.sync(context.Background()); err == nil {
		t.Fatalf("expected sync error")
	}
}

func TestPeriodicSyncerStartStop(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.json")
	var calls atomic.Int32
	src := staticSource{clients: []Client{{ID: "uuid-a"}}}
	opts := PeriodicOptions{
		Interval: 10 * time.Millisecond,
		Source: clientSourceFunc(func(ctx context.Context) ([]Client, error) {
			calls.Add(1)
			return src.ListClients(ctx)
		}),
		Generators: []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
	}
	syncer, err := NewPeriodicSyncer(opts)
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	stop, err := syncer.Start(context.Background())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	deadline := time.Now().Add(200 * time.Millisecond)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if calls.Load() == 0 {
		t.Fatalf("sync never executed")
	}
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
}

func TestPeriodicSyncerOnSyncCallback(t *testing.T) {
	output := filepath.Join(t.TempDir(), "config.json")
	var results []SyncResult
	var mu sync.Mutex
	opts := PeriodicOptions{
		Interval:   5 * time.Millisecond,
		Source:     staticSource{clients: []Client{{ID: "uuid-a"}}},
		Generators: []Generator{{Definition: JSONDefinition{Raw: []byte(minimalTemplate)}, OutputPath: output}},
		OnSync: func(res SyncResult) {
			mu.Lock()
			defer mu.Unlock()
			results = append(results, res)
		},
	}
	syncer, err := NewPeriodicSyncer(opts)
	if err != nil {
		t.Fatalf("new syncer: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop, err := syncer.Start(ctx)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	if err := stop(context.Background()); err != nil {
		t.Fatalf("stop: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(results) == 0 {
		t.Fatalf("expected at least one sync result")
	}
	if results[0].Clients != 1 {
		t.Fatalf("expected 1 client, got %d", results[0].Clients)
	}
	if results[0].CompletedAt.IsZero() {
		t.Fatalf("expected completion timestamp to be set")
	}
}

type clientSourceFunc func(ctx context.Context) ([]Client, error)

func (f clientSourceFunc) ListClients(ctx context.Context) ([]Client, error) {
	return f(ctx)
}
