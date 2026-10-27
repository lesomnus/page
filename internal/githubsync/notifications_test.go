package githubsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func notificationSource(t *testing.T, cat *deploy.Catalog, server string, poll time.Duration) *Sync {
	t.Helper()
	s, err := New(cat, Config{ID: "source", Owner: "acme", APIURL: server, PollInterval: poll, RepositoryPattern: `^([a-z0-9][a-z0-9-]*)\.hday\.page$`})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func awaitNotification(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("notification did not cause a reconciliation")
	}
}

func runNotificationSource(t *testing.T, s *Sync) context.CancelFunc {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Errorf("run returned %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("notification loop did not stop")
		}
	})
	return cancel
}

func TestNotifyRepositoryFiltersAndCoalesces(t *testing.T) {
	s := notificationSource(t, deploy.New(storage.NewMemory()), "https://example.com", time.Hour)
	for _, full := range []string{"other/foo.hday.page", "acme/homepage", "acme/foo.app", "acme/foo.hday.page/extra", "foo.hday.page", "acme/.hday.page"} {
		if s.NotifyRepository(full) || len(s.wake) != 0 {
			t.Fatalf("unrelated repository queued: %q", full)
		}
	}
	for i := 0; i < 1000; i++ {
		if !s.NotifyRepository("ACME/foo.hday.page") {
			t.Fatal("matching repository was ignored")
		}
	}
	if len(s.wake) != 1 {
		t.Fatal("repository notifications were not coalesced")
	}
}

func TestNotifyDuringPassQueuesOneFollowup(t *testing.T) {
	started := make(chan struct{}, 3)
	release := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		started <- struct{}{}
		if n <= 2 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		w.Write([]byte("[]"))
	}))
	defer server.Close()
	s := notificationSource(t, deploy.New(storage.NewMemory()), server.URL, time.Hour)
	runNotificationSource(t, s)
	awaitNotification(t, started)
	for i := 0; i < 1000; i++ {
		s.Notify()
	}
	release <- struct{}{}
	awaitNotification(t, started)
	for i := 0; i < 1000; i++ {
		s.Notify()
	}
	release <- struct{}{}
	awaitNotification(t, started)
	time.Sleep(2 * notificationDebounce)
	if got := calls.Load(); got != 3 {
		t.Fatalf("expected startup and two coalesced followups, got %d", got)
	}
}

func TestSourcePollingAndRetryRecoverWithoutAnotherEvent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		poll   time.Duration
		status int
	}{
		{"periodic", 30 * time.Millisecond, http.StatusOK},
		{"transient retry", time.Hour, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan struct{}, 10)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- struct{}{}
				if calls.Add(1) == 1 && tc.status != http.StatusOK {
					w.WriteHeader(tc.status)
					return
				}
				w.Write([]byte("[]"))
			}))
			defer server.Close()
			s := notificationSource(t, deploy.New(storage.NewMemory()), server.URL, tc.poll)
			runNotificationSource(t, s)
			awaitNotification(t, seen)
			awaitNotification(t, seen)
		})
	}
}

func TestPermissionFailureWaitsForNotificationOrPolling(t *testing.T) {
	seen := make(chan struct{}, 10)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		seen <- struct{}{}
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()
	s := notificationSource(t, deploy.New(storage.NewMemory()), server.URL, time.Hour)
	runNotificationSource(t, s)
	awaitNotification(t, seen)
	time.Sleep(syncRetryInitial + 100*time.Millisecond)
	if calls.Load() != 1 {
		t.Fatal("permission failure was retried without a new event")
	}
	s.Notify()
	awaitNotification(t, seen)
}

func TestControlWriteWakesOwningSource(t *testing.T) {
	_, server := fixture(t)
	cat := deploy.New(storage.NewMemory())
	s, err := New(cat, Config{ID: "source", Owner: "acme", APIURL: server.URL, PollInterval: time.Hour, Branches: []string{"main"}, RecentCommits: 1})
	if err != nil {
		t.Fatal(err)
	}
	runNotificationSource(t, s)
	deadline := time.Now().Add(3 * time.Second)
	for {
		site, err := cat.GetSite(context.Background(), "foo")
		if err == nil && site.Production != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("startup publication did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// Wait for the startup pass to leave the source writer lock, then write
	// only operator intent. The loop must wake and materialize that intent.
	s.mu.Lock()
	s.mu.Unlock()
	if _, err := cat.RequestControl(context.Background(), "foo", "source", "disabled", ""); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(3 * time.Second)
	for {
		site, err := cat.GetSite(context.Background(), "foo")
		if err == nil && site.Disabled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("control notification did not disable the publication")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSourceCancellationInterruptsInflightRequest(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()
	s := notificationSource(t, deploy.New(storage.NewMemory()), server.URL, time.Hour)
	cancel := runNotificationSource(t, s)
	awaitNotification(t, started)
	cancel()
}

func TestRetryClassification(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{{nil, false}, {context.Canceled, false}, {&apiError{status: 401}, false}, {&apiError{status: 403}, false}, {&apiError{status: 429}, true}, {&apiError{status: 500}, true}, {errors.New("storage unavailable"), true}, {errors.Join(errors.New("control"), &apiError{status: 503}), true}} {
		if got := retrySource(tc.err); got != tc.want {
			t.Fatalf("retry %v: got %t want %t", tc.err, got, tc.want)
		}
	}
}

func TestSourceFastRetriesStopUntilAnotherTrigger(t *testing.T) {
	// Invalid source content need not have a typed HTTP error. Even such an
	// enduring error gets only this short recovery window, then normal polling.
	err := errors.New("invalid source content")
	var retries sourceRetries
	retries.reset()
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		got, ok := retries.next(err)
		if !ok || got != want {
			t.Fatalf("fast retry: got %s/%t want %s/true", got, ok, want)
		}
	}
	for i := 0; i < 10; i++ {
		if delay, ok := retries.next(err); ok || delay != 0 {
			t.Fatalf("exhausted trigger kept retrying: %s/%t", delay, ok)
		}
	}
	// A new hint, periodic pass or successful pass starts a fresh window.
	retries.reset()
	if delay, ok := retries.next(err); !ok || delay != time.Second {
		t.Fatalf("new trigger did not reset retries: %s/%t", delay, ok)
	}
	retries.reset()
	if _, ok := retries.next(&apiError{status: http.StatusForbidden}); ok {
		t.Fatal("bounded retry window made a permission error retryable")
	}
}
