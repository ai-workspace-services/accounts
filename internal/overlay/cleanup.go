package overlay

import (
	"context"
	"time"
)

const (
	// DefaultEnrollmentRetention keeps expired enrollment sessions for 24 hours
	// for post-mortem debugging before pruning them from the database.
	DefaultEnrollmentRetention = 24 * time.Hour

	// DefaultSessionCleanupInterval defines how often the background pruner
	// sweeps expired overlay enrollment sessions.
	DefaultSessionCleanupInterval = time.Hour
)

// PruneExpiredEnrollmentSessions deletes expired enrollment session rows older
// than cutoff (expires_at < cutoff) using the expires_at index.
func (r *Repository) PruneExpiredEnrollmentSessions(ctx context.Context, cutoff time.Time) (int64, error) {
	result := r.DB.WithContext(ctx).
		Where("expires_at < ?", cutoff.UTC()).
		Delete(&EnrollmentRecord{})
	return result.RowsAffected, result.Error
}

// PruneExpiredEnrollmentSessions removes enrollment sessions that have been
// expired for at least retention duration.
func (s *Service) PruneExpiredEnrollmentSessions(ctx context.Context, retention time.Duration) (int64, error) {
	if retention <= 0 {
		retention = DefaultEnrollmentRetention
	}
	cutoff := s.now().Add(-retention)
	return s.repo.PruneExpiredEnrollmentSessions(ctx, cutoff)
}

// StartSessionCleanup periodically prunes expired overlay_enrollment_sessions
// records until ctx is canceled.
func (s *Service) StartSessionCleanup(ctx context.Context, interval, retention time.Duration, onResult func(deleted int64, err error)) {
	if interval <= 0 {
		interval = DefaultSessionCleanupInterval
	}
	if retention <= 0 {
		retention = DefaultEnrollmentRetention
	}
	go func() {
		runOnce := func() {
			pruneCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			deleted, err := s.PruneExpiredEnrollmentSessions(pruneCtx, retention)
			cancel()
			if onResult != nil {
				onResult(deleted, err)
			}
		}

		// Sweep once shortly after startup so long-accumulated expired rows are reclaimed.
		initTimer := time.NewTimer(15 * time.Second)
		select {
		case <-ctx.Done():
			initTimer.Stop()
			return
		case <-initTimer.C:
			runOnce()
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				runOnce()
			}
		}
	}()
}
