package githubsync

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/deploy"
)

const (
	notificationDebounce = 150 * time.Millisecond
	syncRetryInitial     = time.Second
	syncFastRetries      = 3
)

// Notify queues a source reconciliation without blocking the caller. One queued
// hint is sufficient: a pass reads current source state, not an event's revision.
func (s *Sync) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// NotifyRepository queues only repositories covered by this source. A matching
// webhook still does not bypass manifest, ref-policy or ownership validation.
func (s *Sync) NotifyRepository(fullName string) bool {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || !strings.EqualFold(owner, s.cfg.Owner) || !validRepository(fullName) {
		return false
	}
	match := s.pattern.FindStringSubmatch(name)
	if match == nil || !deploy.ValidName(strings.ToLower(match[1])) {
		return false
	}
	s.Notify()
	return true
}

func retrySource(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var api *apiError
	if errors.As(err, &api) && api.status >= 400 && api.status < 500 && api.status != http.StatusRequestTimeout && api.status != http.StatusTooManyRequests {
		// Permission/configuration errors need a new event or the periodic pass,
		// rather than repeatedly querying GitHub with unchanged credentials.
		return false
	}
	return true
}

// sourceRetries bounds accelerated recovery after one trigger. Some source
// errors are permanent without having a typed API status (for example a bad
// manifest), so they must not cause full organization scans every minute forever.
type sourceRetries struct {
	remaining int
	delay     time.Duration
}

func (r *sourceRetries) reset() {
	r.remaining, r.delay = syncFastRetries, syncRetryInitial
}

func (r *sourceRetries) next(err error) (time.Duration, bool) {
	if r.remaining == 0 || !retrySource(err) {
		return 0, false
	}
	delay := r.delay
	r.remaining--
	r.delay *= 2
	return delay, true
}

func drainHints(ch <-chan struct{}) bool {
	// Both queues have capacity one. Consume only that queued hint so a
	// continuous producer cannot keep the writer busy draining indefinitely.
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func (s *Sync) run(ctx context.Context) error {
	// Register before the startup pass so a control write during that pass
	// remains queued for a follow-up reconciliation.
	controls, unsubscribe := s.cat.WatchControls()
	defer unsubscribe()
	poll := time.NewTicker(s.cfg.PollInterval)
	defer poll.Stop()
	var timer *time.Timer
	var scheduled <-chan time.Time
	var due time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	schedule := func(delay time.Duration) {
		when := time.Now().Add(delay)
		if scheduled != nil && !when.Before(due) {
			return
		}
		if timer == nil {
			timer = time.NewTimer(delay)
		} else {
			timer.Reset(delay)
		}
		scheduled, due = timer.C, when
	}
	var retries sourceRetries
	retries.reset()
	reconcile := func() {
		if timer != nil {
			timer.Stop()
		}
		scheduled = nil
		// Hints already queued before this pass are covered by this pass. Any
		// hint arriving while Once runs stays queued for the next pass.
		sourceHint := drainHints(s.wake)
		controlHint := drainHints(controls)
		if sourceHint || controlHint {
			retries.reset()
		}
		err := s.Once(ctx)
		if err != nil && ctx.Err() == nil {
			slog.ErrorContext(ctx, "GitHub sync failed", "source", s.cfg.ID, "error", err)
			if delay, ok := retries.next(err); ok {
				schedule(delay)
			}
		} else if err == nil {
			retries.reset()
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	reconcile()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			retries.reset()
			reconcile()
		case <-s.wake:
			retries.reset()
			schedule(notificationDebounce)
		case _, ok := <-controls:
			if !ok {
				controls = nil
				continue
			}
			retries.reset()
			schedule(notificationDebounce)
		case <-scheduled:
			reconcile()
		}
	}
}
