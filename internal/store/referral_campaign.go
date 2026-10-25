package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

var ErrReferralCampaignInvalid = errors.New("invalid referral campaign configuration")

// ReferralCampaign is an immutable revision of the single XConnect campaign.
// Bounds are inclusive start and exclusive end; nil means no bound.
type ReferralCampaign struct {
	Revision    int64      `json:"revision"`
	Enabled     bool       `json:"enabled"`
	StartsAt    *time.Time `json:"startsAt"`
	EndsAt      *time.Time `json:"endsAt"`
	EffectiveAt time.Time  `json:"effectiveAt"`
	ActorUUID   string     `json:"actorId,omitempty"`
	Reason      string     `json:"reason"`
}

func (c *ReferralCampaign) Active(at time.Time) bool {
	return c != nil && c.Enabled && (c.StartsAt == nil || !at.Before(*c.StartsAt)) && (c.EndsAt == nil || at.Before(*c.EndsAt))
}

// ReferralCampaignStore keeps campaign configuration separate from the broad
// account Store contract. Changes always append; effective time is server-owned.
type ReferralCampaignStore interface {
	GetReferralCampaign(context.Context, time.Time) (*ReferralCampaign, error)
	ConfigureReferralCampaign(context.Context, *ReferralCampaign) (*ReferralCampaign, error)
	GetReferralRewardCampaign(context.Context, string) (*ReferralCampaign, error)
}

func defaultReferralCampaign() ReferralCampaign {
	zone := time.FixedZone("Asia/Shanghai", 8*60*60)
	start, end := time.Date(2026, 10, 13, 0, 0, 0, 0, zone).UTC(), time.Date(2027, 1, 1, 0, 0, 0, 0, zone).UTC()
	return ReferralCampaign{Revision: 1, Enabled: true, StartsAt: &start, EndsAt: &end, EffectiveAt: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), Reason: "initial XConnect campaign defaults"}
}

func cloneReferralCampaign(c ReferralCampaign) *ReferralCampaign {
	if c.StartsAt != nil {
		value := c.StartsAt.UTC()
		c.StartsAt = &value
	}
	if c.EndsAt != nil {
		value := c.EndsAt.UTC()
		c.EndsAt = &value
	}
	return &c
}

func validateReferralCampaign(c *ReferralCampaign) error {
	if c == nil || strings.TrimSpace(c.Reason) == "" || len(strings.TrimSpace(c.Reason)) > 500 || (c.StartsAt != nil && c.EndsAt != nil && !c.StartsAt.Before(*c.EndsAt)) {
		return ErrReferralCampaignInvalid
	}
	return nil
}

func (s *memoryStore) referralCampaignAt(at time.Time) *ReferralCampaign {
	var selected *ReferralCampaign
	for _, c := range s.referralCampaigns {
		if !c.EffectiveAt.After(at) && (selected == nil || c.EffectiveAt.After(selected.EffectiveAt) || (c.EffectiveAt.Equal(selected.EffectiveAt) && c.Revision > selected.Revision)) {
			selected = cloneReferralCampaign(c)
		}
	}
	return selected
}

func (s *memoryStore) GetReferralCampaign(_ context.Context, at time.Time) (*ReferralCampaign, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c := s.referralCampaignAt(at)
	if c == nil {
		return nil, ErrReferralNotFound
	}
	return c, nil
}

func (s *memoryStore) ConfigureReferralCampaign(_ context.Context, input *ReferralCampaign) (*ReferralCampaign, error) {
	if err := validateReferralCampaign(input); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c := cloneReferralCampaign(*input)
	c.Revision = int64(len(s.referralCampaigns) + 1)
	c.EffectiveAt = time.Now().UTC()
	if last := s.referralCampaigns[len(s.referralCampaigns)-1]; !c.EffectiveAt.After(last.EffectiveAt) {
		c.EffectiveAt = last.EffectiveAt.Add(time.Microsecond)
	}
	c.Reason = strings.TrimSpace(c.Reason)
	c.ActorUUID = strings.TrimSpace(c.ActorUUID)
	s.referralCampaigns = append(s.referralCampaigns, *c)
	return cloneReferralCampaign(*c), nil
}

func (s *memoryStore) GetReferralRewardCampaign(_ context.Context, rewardID string) (*ReferralCampaign, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	c, ok := s.referralRewardCampaigns[rewardID]
	if !ok {
		return nil, ErrReferralNotFound
	}
	return cloneReferralCampaign(c), nil
}

type referralCampaignScanner interface{ Scan(...any) error }

func scanReferralCampaign(row referralCampaignScanner) (*ReferralCampaign, error) {
	var c ReferralCampaign
	var start, end sql.NullTime
	var actor sql.NullString
	if err := row.Scan(&c.Revision, &c.Enabled, &start, &end, &c.EffectiveAt, &actor, &c.Reason); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrReferralNotFound
		}
		return nil, err
	}
	if start.Valid {
		c.StartsAt = &start.Time
	}
	if end.Valid {
		c.EndsAt = &end.Time
	}
	c.ActorUUID = actor.String
	return cloneReferralCampaign(c), nil
}

const referralCampaignColumns = `revision,enabled,starts_at,ends_at,effective_at,actor_uuid::text,reason`

func (s *postgresStore) GetReferralCampaign(ctx context.Context, at time.Time) (*ReferralCampaign, error) {
	return scanReferralCampaign(s.db.QueryRowContext(ctx, `SELECT `+referralCampaignColumns+` FROM public.referral_campaign_revisions WHERE effective_at<=$1 ORDER BY effective_at DESC,revision DESC LIMIT 1`, at.UTC()))
}

func (s *postgresStore) ConfigureReferralCampaign(ctx context.Context, input *ReferralCampaign) (*ReferralCampaign, error) {
	if err := validateReferralCampaign(input); err != nil {
		return nil, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended('xconnect-referral-campaign',0))`); err != nil {
		return nil, err
	}
	c, err := scanReferralCampaign(tx.QueryRowContext(ctx, `INSERT INTO public.referral_campaign_revisions(enabled,starts_at,ends_at,effective_at,actor_uuid,reason)
 SELECT $1,$2,$3,GREATEST(clock_timestamp(),COALESCE(MAX(effective_at)+interval '1 microsecond','1970-01-01'::timestamptz)),NULLIF($4,'')::uuid,$5 FROM public.referral_campaign_revisions RETURNING `+referralCampaignColumns, input.Enabled, input.StartsAt, input.EndsAt, strings.TrimSpace(input.ActorUUID), strings.TrimSpace(input.Reason)))
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	return c, nil
}

func (s *postgresStore) GetReferralRewardCampaign(ctx context.Context, rewardID string) (*ReferralCampaign, error) {
	return scanReferralCampaign(s.db.QueryRowContext(ctx, `SELECT `+referralCampaignColumns+` FROM public.referral_campaign_revisions WHERE revision=(SELECT campaign_revision FROM public.referral_reward_campaign_snapshots WHERE reward_id=$1)`, rewardID))
}

var _ ReferralCampaignStore = (*memoryStore)(nil)
var _ ReferralCampaignStore = (*postgresStore)(nil)
