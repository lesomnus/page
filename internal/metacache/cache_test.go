package metacache

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

type testClock struct{ now atomic.Int64 }

func newClock() *testClock                   { c := &testClock{}; c.now.Store(time.Now().UnixNano()); return c }
func (c *testClock) Time() time.Time         { return time.Unix(0, c.now.Load()) }
func (c *testClock) Advance(d time.Duration) { c.now.Add(int64(d)) }
func testCache(t *testing.T, cfg Config) *Cache {
	t.Helper()
	c, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
func valuePolicy(loads *atomic.Int64, mutable bool) Policy {
	return Policy{Mutable: mutable, Load: func(context.Context) ([]byte, string, error) { loads.Add(1); return []byte("payload"), "etag", nil }, Decode: func(data []byte) (any, error) { return string(data), nil }}
}
func get(t *testing.T, c *Cache, key string, p Policy) any {
	t.Helper()
	v, err := c.Get(context.Background(), key, p)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestMutableFreshnessRetainsAndCoalescesExpiredRevalidation(t *testing.T) {
	clock := newClock()
	c := testCache(t, Config{Now: clock.Time, TTL: time.Second})
	var loads, checks atomic.Int64
	p := valuePolicy(&loads, true)
	began, release := make(chan struct{}), make(chan struct{})
	p.Revalidate = func(context.Context, string) (bool, error) {
		if checks.Add(1) == 1 {
			close(began)
		}
		<-release
		return true, nil
	}
	if got := get(t, c, "site", p); got != "payload" {
		t.Fatal(got)
	}
	for range 10 {
		get(t, c, "site", p)
	}
	if loads.Load() != 1 || checks.Load() != 0 {
		t.Fatal("fresh value caused origin work")
	}
	clock.Advance(time.Second)
	if s := c.Stats(); s.Entries != 1 {
		t.Fatal("TTL purged value", s)
	}
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			if v, err := c.Get(context.Background(), "site", p); err != nil || v != "payload" {
				t.Errorf("revalidate: %v %v", v, err)
			}
		})
	}
	<-began
	close(release)
	wg.Wait()
	if loads.Load() != 1 || checks.Load() != 1 {
		t.Fatalf("coalescing lost: GET=%d HEAD=%d", loads.Load(), checks.Load())
	}
	if s := c.Stats(); s.Entries != 1 {
		t.Fatal(s)
	}
}

func TestDiskRehydratesEvictedImmutableValuesAndSurvivesRestart(t *testing.T) {
	cfg := Config{Directory: t.TempDir(), Namespace: "bucket/prefix", MemoryBytes: 5000, DiskBytes: 16384}
	c := testCache(t, cfg)
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	get(t, c, "a", p)
	get(t, c, "b", p)
	if s := c.Stats(); s.Entries != 1 {
		t.Fatal(s)
	}
	if got := get(t, c, "a", p); got != "payload" || loads.Load() != 2 {
		t.Fatalf("disk promotion fetched origin: %v %d", got, loads.Load())
	}
	restarted := testCache(t, cfg)
	if got := get(t, restarted, "a", p); got != "payload" || loads.Load() != 2 {
		t.Fatalf("restart lost disk: %v %d", got, loads.Load())
	}
	info, err := os.Stat(c.diskPath(c.identity("a", p)))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private file: %v %v", info, err)
	}
	dir, err := os.Stat(c.directory)
	if err != nil || dir.Mode().Perm() != 0700 {
		t.Fatalf("private directory: %v %v", dir, err)
	}
}

func TestMutableDiskPromotionRevalidatesEvenInsideTTL(t *testing.T) {
	cfg := Config{Directory: t.TempDir(), Namespace: "bucket", TTL: time.Hour}
	var loads, checks atomic.Int64
	p := valuePolicy(&loads, true)
	p.Revalidate = func(context.Context, string) (bool, error) { checks.Add(1); return true, nil }
	get(t, testCache(t, cfg), "site", p)
	second := testCache(t, cfg)
	get(t, second, "site", p)
	if loads.Load() != 1 || checks.Load() != 1 {
		t.Fatalf("disk freshness was trusted or GET repeated: GET%d HEAD%d", loads.Load(), checks.Load())
	}
	second.InvalidateMutable()
	p.Revalidate = func(context.Context, string) (bool, error) { checks.Add(1); return false, nil }
	get(t, second, "site", p)
	if loads.Load() != 2 || checks.Load() != 2 {
		t.Fatalf("changed source was not fetched: GET%d HEAD%d", loads.Load(), checks.Load())
	}
}

func TestErrorsDoNotAuthorizeStaleValueAndKeepForRecovery(t *testing.T) {
	clock := newClock()
	c := testCache(t, Config{Now: clock.Time, TTL: time.Second, Directory: t.TempDir()})
	var loads atomic.Int64
	p := valuePolicy(&loads, true)
	fail := true
	p.Revalidate = func(context.Context, string) (bool, error) {
		if fail {
			return false, errors.New("source unavailable")
		}
		return true, nil
	}
	get(t, c, "site", p)
	clock.Advance(time.Second)
	if _, err := c.Get(context.Background(), "site", p); err == nil {
		t.Fatal("expired value served on failed validation")
	}
	if c.Stats().Entries != 1 {
		t.Fatal("validation failure deleted old data")
	}
	fail = false
	get(t, c, "site", p)
	if loads.Load() != 1 {
		t.Fatal("recovery discarded revalidatable value")
	}
}

func TestEmptyETagReloadsAndNotFoundIsBrieflyCached(t *testing.T) {
	clock := newClock()
	c := testCache(t, Config{Now: clock.Time, TTL: time.Second, Directory: t.TempDir()})
	var loads, checks atomic.Int64
	p := Policy{Mutable: true, Load: func(context.Context) ([]byte, string, error) { loads.Add(1); return []byte("value"), "", nil }, Decode: func(b []byte) (any, error) { return string(b), nil }, Revalidate: func(context.Context, string) (bool, error) { checks.Add(1); return true, nil }}
	get(t, c, "site", p)
	clock.Advance(time.Second)
	get(t, c, "site", p)
	if loads.Load() != 2 || checks.Load() != 0 {
		t.Fatal("empty validator was reused")
	}
	p.Load = func(context.Context) ([]byte, string, error) { loads.Add(1); return nil, "", storage.ErrNotFound }
	c.InvalidateMutable()
	for range 2 {
		if _, err := c.Get(context.Background(), "site", p); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if loads.Load() != 3 {
		t.Fatal("404 not coalesced by freshness")
	}
	if _, err := os.Stat(c.diskPath(c.identity("site", p))); !os.IsNotExist(err) {
		t.Fatal("deleted Site remained on disk", err)
	}
	clock.Advance(time.Second)
	if _, err := c.Get(context.Background(), "site", p); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	if loads.Load() != 4 {
		t.Fatal("404 retained permanently")
	}
}

func TestInvalidationSeparatesFlightsAndGuardsMemoryAndDisk(t *testing.T) {
	cfg := Config{Directory: t.TempDir(), Namespace: "origin", TTL: time.Hour}
	c := testCache(t, cfg)
	started, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int64
	p := Policy{Mutable: true, Load: func(context.Context) ([]byte, string, error) {
		n := loads.Add(1)
		if n == 1 {
			close(started)
			<-release
			return []byte("old"), "old", nil
		}
		return []byte("new"), "new", nil
	}, Decode: func(b []byte) (any, error) { return string(b), nil }, Revalidate: func(context.Context, string) (bool, error) { return true, nil }}
	old := make(chan any, 1)
	go func() {
		v, err := c.Get(context.Background(), "site", p)
		if err != nil {
			old <- err
		} else {
			old <- v
		}
	}()
	<-started
	c.InvalidateMutable()
	if got := get(t, c, "site", p); got != "new" {
		t.Fatal("new request joined old flight", got)
	}
	close(release)
	if got := <-old; got != "old" {
		t.Fatal(got)
	}
	if got := get(t, c, "site", p); got != "new" {
		t.Fatal("old flight replaced memory", got)
	}
	restarted := testCache(t, cfg)
	if got := get(t, restarted, "site", p); got != "new" || loads.Load() != 2 {
		t.Fatalf("old flight replaced disk: %v %d", got, loads.Load())
	}
}

func TestCanceledWaiterDoesNotCancelSharedLoad(t *testing.T) {
	c := testCache(t, Config{})
	started, release := make(chan struct{}), make(chan struct{})
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	p.Load = func(ctx context.Context) ([]byte, string, error) {
		loads.Add(1)
		close(started)
		select {
		case <-ctx.Done():
			return nil, "", ctx.Err()
		case <-release:
			return []byte("value"), "etag", nil
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan error, 1)
	go func() { _, err := c.Get(ctx, "key", p); first <- err }()
	<-started
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	second := make(chan any, 1)
	go func() {
		v, err := c.Get(context.Background(), "key", p)
		if err != nil {
			second <- err
		} else {
			second <- v
		}
	}()
	close(release)
	if got := <-second; got != "value" || loads.Load() != 1 {
		t.Fatalf("waiter canceled shared load: %v %d", got, loads.Load())
	}
}

func TestBoundsNamespaceCorruptionAndOrphanCleanup(t *testing.T) {
	clock := newClock()
	cfg := Config{Directory: t.TempDir(), Namespace: "one", Now: clock.Time, MemoryBytes: 9000, DiskBytes: 8192}
	c := testCache(t, cfg)
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	for i := range 10 {
		get(t, c, fmt.Sprint(i), p)
		clock.Advance(time.Second)
		s := c.Stats()
		if s.MemoryBytes > cfg.MemoryBytes || s.DiskBytes > cfg.DiskBytes || s.Entries > 2 {
			t.Fatal("cache exceeded accounted bounds", s)
		}
	}
	other := testCache(t, Config{Directory: cfg.Directory, Namespace: "two"})
	get(t, other, "9", p)
	if loads.Load() != 11 {
		t.Fatal("cache namespace crossed origins")
	}
	path := c.diskPath(c.identity("9", p))
	if err := os.WriteFile(path, []byte("{broken"), 0600); err != nil {
		t.Fatal(err)
	}
	restarted := testCache(t, cfg)
	get(t, restarted, "9", p)
	if loads.Load() != 12 {
		t.Fatal("corrupt disk did not fall back to origin")
	}
	orphan := filepath.Join(c.directory, ".metadata-crashed")
	if err := os.WriteFile(orphan, []byte("abandoned"), 0600); err != nil {
		t.Fatal(err)
	}
	testCache(t, cfg)
	if _, err := os.Stat(orphan); !os.IsNotExist(err) {
		t.Fatal("crashed temp file retained", err)
	}
}

func TestOversizeAndBadDiskDoNotPreventOriginResponse(t *testing.T) {
	root := t.TempDir()
	blocked := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blocked, []byte("file"), 0600); err != nil {
		t.Fatal(err)
	}
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	c := testCache(t, Config{Directory: blocked, MemoryBytes: 1, DiskBytes: 1})
	get(t, c, "a", p)
	get(t, c, "a", p)
	if s := c.Stats(); s.Entries != 0 || s.MemoryBytes != 0 || s.DiskBytes != 0 || loads.Load() != 2 {
		t.Fatal(s, loads.Load())
	}
	cfg := Config{Directory: t.TempDir(), DiskBytes: 1}
	disk := testCache(t, cfg)
	get(t, disk, "key", p)
	entries, err := os.ReadDir(disk.directory)
	if err != nil || len(entries) != 0 {
		t.Fatal("oversize disk entry retained", entries, err)
	}
}

func TestBoundedDiskReadRejectsSparseOversizeAndCopiedRecords(t *testing.T) {
	cfg := Config{Directory: t.TempDir(), Namespace: "one", DiskBytes: 1 << 30}
	c := testCache(t, cfg)
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	get(t, c, "a", p)
	a := c.diskPath(c.identity("a", p))
	b := c.diskPath(c.identity("b", p))
	encoded, err := os.ReadFile(a)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(b, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	if got := get(t, c, "b", p); got != "payload" || loads.Load() != 2 {
		t.Fatal("wrong-key disk record reused", got, loads.Load())
	}
	large := c.diskPath(c.identity("large", p))
	file, err := os.Create(large)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxDiskRecordBytes + 1); err != nil {
		t.Fatal(err)
	}
	file.Close()
	get(t, c, "large", p)
	if loads.Load() != 3 {
		t.Fatal("oversize record read")
	}
}

func TestInvalidConfigurationAndInvalidDecodedData(t *testing.T) {
	for _, cfg := range []Config{{MemoryBytes: -1}, {DiskBytes: -1}, {TTL: -1}} {
		if _, err := New(cfg); err == nil {
			t.Fatal("invalid config accepted", cfg)
		}
	}
	cfg, err := (Config{}).Normalize()
	if err != nil || cfg.TTL != 5*time.Second || cfg.MemoryBytes != 4<<20 || cfg.DiskBytes != 64<<20 {
		t.Fatal(cfg, err)
	}
	c := testCache(t, Config{Directory: t.TempDir()})
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	p.Decode = func([]byte) (any, error) { return nil, errors.New("invalid metadata") }
	for range 2 {
		if _, err := c.Get(context.Background(), "key", p); err == nil || !strings.Contains(err.Error(), "invalid metadata") {
			t.Fatal(err)
		}
	}
	if s := c.Stats(); s.Entries != 0 || s.DiskBytes != 0 || loads.Load() != 2 {
		t.Fatal(s, loads.Load())
	}
}
