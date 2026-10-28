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
	t.Cleanup(c.Close)
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

func TestIdleTTLSlidesAndDiskReuseNeverCallsOrigin(t *testing.T) {
	for _, mutable := range []bool{false, true} {
		t.Run(fmt.Sprint(mutable), func(t *testing.T) {
			clock := newClock()
			c := testCache(t, Config{Now: clock.Time, TTL: time.Minute, Directory: t.TempDir()})
			var loads atomic.Int64
			p := valuePolicy(&loads, mutable)
			get(t, c, "key", p)
			for range 20 {
				clock.Advance(10 * time.Second)
				get(t, c, "key", p)
			}
			if loads.Load() != 1 || c.Stats().Entries != 1 {
				t.Fatal("active reads expired metadata", loads.Load(), c.Stats())
			}
			clock.Advance(time.Minute)
			c.Sweep()
			if s := c.Stats(); s.Entries != 0 || s.DiskBytes == 0 {
				t.Fatal("idle expiry did not leave only disk", s)
			}
			get(t, c, "key", p)
			if loads.Load() != 1 {
				t.Fatal("disk promotion revalidated or reloaded origin", loads.Load())
			}
		})
	}
}

func TestTimerEvictsIdleRAMWithoutAnyReads(t *testing.T) {
	c := testCache(t, Config{TTL: 20 * time.Millisecond, Directory: t.TempDir()})
	var loads atomic.Int64
	p := valuePolicy(&loads, true)
	get(t, c, "key", p)
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		count := len(c.items)
		c.mu.Unlock()
		if count == 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	c.mu.Lock()
	count := len(c.items)
	c.mu.Unlock()
	if count != 0 || loads.Load() != 1 {
		t.Fatalf("idle janitor failed or did origin work: entries%d loads%d", count, loads.Load())
	}
	if _, err := os.Stat(c.diskPath(c.identity("key", p))); err != nil {
		t.Fatal("idle eviction deleted disk", err)
	}
	get(t, c, "key", p)
	if loads.Load() != 1 {
		t.Fatal("idle disk promotion accessed origin")
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

func TestStartupPurgesMutableDiskButKeepsImmutable(t *testing.T) {
	cfg := Config{Directory: t.TempDir(), Namespace: "bucket", TTL: time.Hour}
	var loads atomic.Int64
	mutable := valuePolicy(&loads, true)
	immutable := valuePolicy(&loads, false)
	first := testCache(t, cfg)
	get(t, first, "site", mutable)
	get(t, first, "release", immutable)
	second := testCache(t, cfg)
	get(t, second, "site", mutable)
	get(t, second, "release", immutable)
	if loads.Load() != 3 {
		t.Fatal("startup reused mutable data or discarded immutable", loads.Load())
	}
	second.InvalidateMutable()
	get(t, second, "site", mutable)
	if loads.Load() != 4 {
		t.Fatal("invalidation resurrected mutable disk", loads.Load())
	}
}

func TestNegativeCacheIdleExpiryAndPublicationInvalidation(t *testing.T) {
	clock := newClock()
	c := testCache(t, Config{Now: clock.Time, TTL: time.Second, Directory: t.TempDir()})
	var loads atomic.Int64
	p := valuePolicy(&loads, true)
	p.Load = func(context.Context) ([]byte, string, error) { loads.Add(1); return nil, "", storage.ErrNotFound }
	for range 2 {
		if _, err := c.Get(context.Background(), "missing", p); !errors.Is(err, storage.ErrNotFound) {
			t.Fatal(err)
		}
	}
	if loads.Load() != 1 {
		t.Fatal("404 not shared")
	}
	clock.Advance(time.Second)
	c.Sweep()
	if _, err := c.Get(context.Background(), "missing", p); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal(err)
	}
	if loads.Load() != 2 {
		t.Fatal("idle404 persisted")
	}
	c.InvalidateMutable()
	p = valuePolicy(&loads, true)
	get(t, c, "missing", p)
	if loads.Load() != 3 {
		t.Fatal("404 blocked publication")
	}
}

func TestBackgroundSeedIsGenerationGuardedAndDoesNotAdmitOrTouchRAM(t *testing.T) {
	clock := newClock()
	c := testCache(t, Config{Directory: t.TempDir(), Now: clock.Time, TTL: time.Minute})
	var loads atomic.Int64
	p := valuePolicy(&loads, true)
	generation := c.Generation()
	c.SeedDisk("site", p, []byte("seed"), "", generation)
	if c.Stats().Entries != 0 {
		t.Fatal("background update admitted RAM")
	}
	if got := get(t, c, "site", p); got != "seed" || loads.Load() != 0 {
		t.Fatal(got, loads.Load())
	}
	clock.Advance(50 * time.Second)
	c.SeedDisk("site", p, []byte("seed"), "", generation)
	clock.Advance(10 * time.Second)
	c.Sweep()
	if c.Stats().Entries != 0 {
		t.Fatal("background update renewed idle timestamp")
	}
	c.InvalidateMutable()
	c.SeedDisk("site", p, []byte("obsolete"), "", generation)
	if got := get(t, c, "site", p); got != "payload" || loads.Load() != 1 {
		t.Fatal("old background update survived invalidation", got, loads.Load())
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
	}, Decode: func(b []byte) (any, error) { return string(b), nil }}
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
	data, _, ok := c.readDisk(c.identity("site", p), true)
	if !ok || string(data) != "new" || loads.Load() != 2 {
		t.Fatalf("old flight replaced disk: %q %v loads%d", data, ok, loads.Load())
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
	if err != nil || cfg.TTL != 5*time.Minute || cfg.MemoryBytes != 4<<20 || cfg.DiskBytes != 64<<20 {
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

func TestConcurrentColdReadsShareOneOriginLoad(t *testing.T) {
	c := testCache(t, Config{})
	var loads atomic.Int64
	p := valuePolicy(&loads, false)
	started, release := make(chan struct{}), make(chan struct{})
	p.Load = func(context.Context) ([]byte, string, error) {
		if loads.Add(1) == 1 {
			close(started)
		}
		<-release
		return []byte("value"), "", nil
	}
	var wg sync.WaitGroup
	for range 30 {
		wg.Go(func() {
			v, err := c.Get(context.Background(), "key", p)
			if err != nil || v != "value" {
				t.Errorf("burst: %v %v", v, err)
			}
		})
	}
	<-started
	close(release)
	wg.Wait()
	if loads.Load() != 1 {
		t.Fatal("burst duplicated origin reads", loads.Load())
	}
}
