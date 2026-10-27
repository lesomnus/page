package githubsync

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/deploy"
)

const (
	notificationDebounce = 150 * time.Millisecond
	syncRetryInitial     = time.Second
	syncFastRetries      = 3
)

// Notify queues a full source reconciliation, as periodic discovery does.
// Repository webhooks use NotifyRepository to avoid scanning other repositories.
func (s *Sync) Notify() {
	s.pendingMu.Lock()
	s.pendingFull = true
	s.pendingMu.Unlock()
	s.signal()
}

func (s *Sync) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// NotifyRepository queues only repositories covered by this source. A matching
// webhook still does not bypass manifest, ref-policy or ownership validation.
func (s *Sync) NotifyRepository(fullName string) bool {
	if _, ok := s.repositorySite(fullName); !ok {
		return false
	}
	s.pendingMu.Lock()
	if s.pendingRepositories == nil {
		s.pendingRepositories = make(map[string]string)
	}
	s.pendingRepositories[strings.ToLower(fullName)] = fullName
	s.pendingMu.Unlock()
	s.signal()
	return true
}

func (s *Sync) repositorySite(fullName string) (string, bool) {
	owner, name, ok := strings.Cut(fullName, "/")
	if !ok || !strings.EqualFold(owner, s.cfg.Owner) || !validRepository(fullName) {
		return "", false
	}
	match := s.pattern.FindStringSubmatch(name)
	if match == nil || !deploy.ValidName(strings.ToLower(match[1])) {
		return "", false
	}
	return strings.ToLower(match[1]), true
}

func (s *Sync) takeNotifications() (bool, []string) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	full := s.pendingFull
	s.pendingFull = false
	keys := make([]string, 0, len(s.pendingRepositories))
	for key := range s.pendingRepositories {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	repositories := make([]string, 0, len(keys))
	for _, key := range keys {
		repositories = append(repositories, s.pendingRepositories[key])
	}
	s.pendingRepositories = nil
	// Hold pendingMu while consuming the signal so a subsequent notification
	// cannot add work and lose its wakeup between snapshot and drain.
	drainHints(s.wake)
	return full, repositories
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
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	jobs := map[string]*syncJob{}
	enqueue := func(kind syncJobKind, repository string, delay time.Duration) {
		key := jobKey(kind, repository)
		due := time.Now().Add(delay)
		if existing := jobs[key]; existing != nil {
			existing.retries.reset()
			if due.Before(existing.due) {
				existing.due = due
			}
			return
		}
		job := &syncJob{kind: kind, repository: repository, due: due}
		job.retries.reset()
		jobs[key] = job
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	enqueue(fullSyncJob, "", 0)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		full, repositories := s.takeNotifications()
		if full {
			enqueue(fullSyncJob, "", notificationDebounce)
		}
		for _, repository := range repositories {
			enqueue(repositorySyncJob, repository, notificationDebounce)
		}
		if drainHints(controls) {
			enqueue(controlSyncJob, "", notificationDebounce)
		}
		// Due repository jobs must not indefinitely starve periodic discovery
		// when a busy source keeps supplying new targeted work.
		select {
		case <-poll.C:
			enqueue(fullSyncJob, "", 0)
		default:
		}
		key, job := nextSyncJob(jobs)
		if job != nil && !time.Now().Before(job.due) {
			delete(jobs, key)
			err := s.executeJob(ctx, job)
			if err != nil && ctx.Err() == nil {
				slog.ErrorContext(ctx, "GitHub sync failed", "source", s.cfg.ID, "repository", job.repository, "error", err)
				if delay, ok := job.retries.next(err); ok {
					job.due = time.Now().Add(delay)
					jobs[key] = job
				}
			}
			continue
		}
		if timer != nil {
			timer.Stop()
		}
		scheduled = nil
		if job != nil {
			delay := max(time.Until(job.due), 0)
			if timer == nil {
				timer = time.NewTimer(delay)
			} else {
				timer.Reset(delay)
			}
			scheduled = timer.C
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-poll.C:
			enqueue(fullSyncJob, "", 0)
		case <-s.wake:
			// Work remains in the protected queue until the top of the loop.
		case _, ok := <-controls:
			if !ok {
				controls = nil
				continue
			}
			enqueue(controlSyncJob, "", notificationDebounce)
		case <-scheduled:
			// The earliest due job is executed at the top of the loop.
		}
	}
}

type syncJobKind uint8

const (
	fullSyncJob syncJobKind = iota
	repositorySyncJob
	controlSyncJob
)

type syncJob struct {
	kind       syncJobKind
	repository string
	due        time.Time
	retries    sourceRetries
}

func jobKey(kind syncJobKind, repository string) string {
	return fmt.Sprintf("%d:%s", kind, strings.ToLower(repository))
}

func nextSyncJob(jobs map[string]*syncJob) (string, *syncJob) {
	var key string
	var next *syncJob
	for candidate, job := range jobs {
		if next == nil || job.due.Before(next.due) || (job.due.Equal(next.due) && candidate < key) {
			key, next = candidate, job
		}
	}
	return key, next
}

func (s *Sync) executeJob(ctx context.Context, job *syncJob) error {
	switch job.kind {
	case fullSyncJob:
		return s.Once(ctx)
	case repositorySyncJob:
		return s.syncNotifiedRepository(ctx, job.repository)
	case controlSyncJob:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.reconcileControls(ctx)
	default:
		return errors.New("unknown source reconciliation job")
	}
}
