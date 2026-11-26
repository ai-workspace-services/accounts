package store

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"time"

	"account/internal/agentproto"

	"github.com/google/uuid"
)

type AgentNodeCapacityLimits struct {
	MonthlyLimitBytes  *int64 `json:"monthlyLimitBytes"`
	BandwidthLimitMbps *int32 `json:"bandwidthLimitMbps"`
}

type AgentNodeCapacity struct {
	AgentNodeCapacityLimits
	Month          string     `json:"month"`
	UsedBytes      *int64     `json:"usedBytes"`
	RemainingBytes *int64     `json:"remainingBytes"`
	ObservedFrom   *time.Time `json:"observedFrom,omitempty"`
	SampledAt      *time.Time `json:"sampledAt,omitempty"`
}

type AgentNodeCapacityStore interface {
	RecordAgentNodeTraffic(ctx context.Context, id string, sample agentproto.NodeTrafficSample) error
	GetAgentNodeCapacity(ctx context.Context, id string, now time.Time) (*AgentNodeCapacity, error)
	SetAgentNodeCapacity(ctx context.Context, id string, limits AgentNodeCapacityLimits, audit *AuditLog) error
}

func agentTrafficMonth(t time.Time) string { return t.UTC().Format("2006-01") }

func agentCounterDelta(previous, sample agentproto.NodeTrafficSample) *int64 {
	// Samples across a month boundary or counter reset cannot be divided
	// reliably. Start from the new baseline and never invent missing usage.
	if previous.SampledAt.IsZero() || agentTrafficMonth(previous.SampledAt) != agentTrafficMonth(sample.SampledAt) ||
		previous.BootID != sample.BootID || previous.Interface != sample.Interface ||
		*previous.RXBytes > *sample.RXBytes || *previous.TXBytes > *sample.TXBytes {
		return nil
	}
	rx := *sample.RXBytes - *previous.RXBytes
	tx := *sample.TXBytes - *previous.TXBytes
	delta := addAgentBytes(rx, tx)
	return &delta
}

func addAgentBytes(a, b int64) int64 {
	if math.MaxInt64-a < b {
		return math.MaxInt64
	}
	return a + b
}

func (capacity *AgentNodeCapacity) setRemaining() {
	if capacity.MonthlyLimitBytes != nil && capacity.UsedBytes != nil {
		remaining := max(0, *capacity.MonthlyLimitBytes-*capacity.UsedBytes)
		capacity.RemainingBytes = &remaining
	}
}

func (s *postgresStore) RecordAgentNodeTraffic(ctx context.Context, id string, sample agentproto.NodeTrafficSample) error {
	sample.SampledAt = sample.SampledAt.UTC().Truncate(time.Microsecond)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts_node_capacity.agent_node_capacity (agent_id) VALUES ($1) ON CONFLICT DO NOTHING`, id); err != nil {
		return err
	}
	var previous agentproto.NodeTrafficSample
	var sampledAt *time.Time
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(interface, ''), COALESCE(boot_id, ''), rx_bytes, tx_bytes, sampled_at
        FROM accounts_node_capacity.agent_node_capacity WHERE agent_id=$1 FOR UPDATE`, id).Scan(
		&previous.Interface, &previous.BootID, &previous.RXBytes, &previous.TXBytes, &sampledAt,
	); err != nil {
		return err
	}
	if sampledAt != nil {
		previous.SampledAt = *sampledAt
		if !sample.SampledAt.After(previous.SampledAt) {
			return nil
		}
	}
	delta := agentCounterDelta(previous, sample)
	if _, err := tx.ExecContext(ctx, `INSERT INTO accounts_node_capacity.agent_node_monthly_traffic
        (agent_id, month, used_bytes, observed_from, sampled_at) VALUES ($1, $2::date, $3, $4, $4)
        ON CONFLICT (agent_id, month) DO UPDATE SET
        used_bytes = CASE
            WHEN EXCLUDED.used_bytes IS NULL THEN agent_node_monthly_traffic.used_bytes
            WHEN agent_node_monthly_traffic.used_bytes IS NULL THEN EXCLUDED.used_bytes
            ELSE agent_node_monthly_traffic.used_bytes +
                LEAST(EXCLUDED.used_bytes, 9223372036854775807 - agent_node_monthly_traffic.used_bytes)
        END,
        sampled_at = EXCLUDED.sampled_at`, id, agentTrafficMonth(sample.SampledAt)+"-01", delta, sample.SampledAt); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts_node_capacity.agent_node_capacity SET interface=$2, boot_id=$3, rx_bytes=$4, tx_bytes=$5, sampled_at=$6
        WHERE agent_id=$1`, id, sample.Interface, sample.BootID, sample.RXBytes, sample.TXBytes, sample.SampledAt); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *postgresStore) GetAgentNodeCapacity(ctx context.Context, id string, now time.Time) (*AgentNodeCapacity, error) {
	capacity := AgentNodeCapacity{Month: agentTrafficMonth(now)}
	err := s.db.QueryRowContext(ctx, `SELECT c.monthly_limit_bytes, c.bandwidth_limit_mbps, t.used_bytes, t.observed_from, t.sampled_at
        FROM accounts_node_capacity.agent_node_capacity c LEFT JOIN accounts_node_capacity.agent_node_monthly_traffic t
        ON t.agent_id=c.agent_id AND t.month=$2::date WHERE c.agent_id=$1`, id, capacity.Month+"-01").Scan(
		&capacity.MonthlyLimitBytes, &capacity.BandwidthLimitMbps, &capacity.UsedBytes, &capacity.ObservedFrom, &capacity.SampledAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	capacity.setRemaining()
	return &capacity, nil
}

func (s *postgresStore) SetAgentNodeCapacity(ctx context.Context, id string, limits AgentNodeCapacityLimits, audit *AuditLog) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(ctx, `INSERT INTO accounts_node_capacity.agent_node_capacity
        (agent_id, monthly_limit_bytes, bandwidth_limit_mbps) VALUES ($1,$2,$3)
        ON CONFLICT (agent_id) DO UPDATE SET monthly_limit_bytes=EXCLUDED.monthly_limit_bytes,
        bandwidth_limit_mbps=EXCLUDED.bandwidth_limit_mbps`, id, limits.MonthlyLimitBytes, limits.BandwidthLimitMbps)
	if err != nil {
		return err
	}
	if err := insertAuditLog(ctx, tx, audit); err != nil {
		return err
	}
	return tx.Commit()
}

type memoryAgentNodeCapacity struct {
	limits AgentNodeCapacityLimits
	last   agentproto.NodeTrafficSample
	months map[string]AgentNodeCapacity
}

func (s *memoryStore) nodeCapacity(id string) *memoryAgentNodeCapacity {
	if s.agentNodeCapacities == nil {
		s.agentNodeCapacities = make(map[string]*memoryAgentNodeCapacity)
	}
	if s.agentNodeCapacities[id] == nil {
		s.agentNodeCapacities[id] = &memoryAgentNodeCapacity{months: make(map[string]AgentNodeCapacity)}
	}
	return s.agentNodeCapacities[id]
}

func (s *memoryStore) RecordAgentNodeTraffic(_ context.Context, id string, sample agentproto.NodeTrafficSample) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	node := s.nodeCapacity(id)
	sample.SampledAt = sample.SampledAt.UTC().Truncate(time.Microsecond)
	if !sample.SampledAt.After(node.last.SampledAt) {
		return nil
	}
	month := agentTrafficMonth(sample.SampledAt)
	capacity := node.months[month]
	if delta := agentCounterDelta(node.last, sample); delta != nil {
		if capacity.UsedBytes != nil {
			*delta = addAgentBytes(*delta, *capacity.UsedBytes)
		}
		capacity.UsedBytes = delta
	}
	if capacity.ObservedFrom == nil {
		capacity.ObservedFrom = &sample.SampledAt
	}
	capacity.SampledAt = &sample.SampledAt
	node.last = sample
	node.months[month] = capacity
	return nil
}

func (s *memoryStore) GetAgentNodeCapacity(_ context.Context, id string, now time.Time) (*AgentNodeCapacity, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	node := s.agentNodeCapacities[id]
	if node == nil {
		return nil, nil
	}
	month := agentTrafficMonth(now)
	capacity := node.months[month]
	capacity.Month = month
	capacity.AgentNodeCapacityLimits = node.limits
	capacity.setRemaining()
	return &capacity, nil
}

func (s *memoryStore) SetAgentNodeCapacity(_ context.Context, id string, limits AgentNodeCapacityLimits, audit *AuditLog) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	audit.UUID = uuid.NewString()
	audit.CreatedAt = time.Now().UTC()
	s.auditLogs = append(s.auditLogs, cloneAuditLog(audit))
	s.nodeCapacity(id).limits = limits
	return nil
}
