// Package metacache provides a bounded read-through cache for small serving
// metadata. Idle expiry evicts memory while retaining the local disk copy. Values returned
// by Get are immutable; only explicit publication invalidation changes metadata.
package metacache

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

const (
	DefaultMemoryBytes = int64(4 << 20)
	DefaultDiskBytes   = int64(64 << 20)
	DefaultTTL         = 5 * time.Minute
	loadTimeout        = 30 * time.Second
)

type Config struct {
	MemoryBytes int64            `yaml:"memory_bytes"`
	DiskBytes   int64            `yaml:"disk_bytes"`
	Directory   string           `yaml:"directory"`
	Namespace   string           `yaml:"-"`
	TTL         time.Duration    `yaml:"ttl"`
	Now         func() time.Time `yaml:"-"`
}

// Normalize supplies defaults without touching the filesystem. Directory=""
// disables the disk tier; each process must own its own local cache directory.
func (cfg Config) Normalize() (Config, error) {
	if cfg.MemoryBytes < 0 || cfg.DiskBytes < 0 || cfg.TTL < 0 {
		return cfg, errors.New("metadata cache capacities and TTL must be nonnegative")
	}
	if cfg.MemoryBytes == 0 {
		cfg.MemoryBytes = DefaultMemoryBytes
	}
	if cfg.DiskBytes == 0 {
		cfg.DiskBytes = DefaultDiskBytes
	}
	if cfg.TTL == 0 {
		cfg.TTL = DefaultTTL
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg, nil
}

type Policy struct {
	Mutable bool
	Load    func(context.Context) (data []byte, etag string, err error)
	Decode  func([]byte) (any, error)
}

type record struct {
	data       []byte
	etag       string
	value      any
	err        error
	accessed   time.Time
	generation uint64
}
type item struct {
	key    string
	record *record
	weight int64
}
type flight struct {
	done  chan struct{}
	value any
	err   error
}

type Cache struct {
	config              Config
	mu                  sync.Mutex
	items               map[string]*list.Element
	lru                 list.List
	used                int64
	generation          uint64
	flights             map[string]*flight
	diskMu              sync.Mutex
	directory           string
	diskUsed            int64
	warnOnce            sync.Once
	mutableDiskDisabled bool
	timer               *time.Timer
	nextExpiry          time.Time
	closed              bool
}

type Stats struct {
	Entries     int
	MemoryBytes int64
	DiskBytes   int64
}

func New(cfg Config) (*Cache, error) {
	var err error
	cfg, err = cfg.Normalize()
	if err != nil {
		return nil, err
	}
	c := &Cache{config: cfg, items: make(map[string]*list.Element), flights: make(map[string]*flight)}
	if cfg.Directory != "" {
		if err := c.openDisk(); err != nil {
			c.warn(err)
			c.directory = ""
		}
	}
	return c, nil
}

// Stats reports conservatively accounted entry weight, not process RSS. The
// weight includes room for decoded maps and compiled route structures. A value
// larger than the budget can still be read, but is not retained in memory.
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	c.sweepLocked()
	c.resetTimerLocked()
	s := Stats{Entries: len(c.items), MemoryBytes: c.used}
	c.mu.Unlock()
	c.diskMu.Lock()
	s.DiskBytes = c.diskUsed
	c.diskMu.Unlock()
	return s
}

// InvalidateMutable removes mutable entries from both tiers. Generation changes
// separate subsequent reads from an old in-flight origin fetch.
func (c *Cache) InvalidateMutable() {
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	c.mu.Lock()
	c.generation++
	for key, element := range c.items {
		if strings.HasPrefix(key, "mutable:") {
			c.removeLocked(element)
		}
	}
	c.resetTimerLocked()
	c.mu.Unlock()
	if err := c.purgeMutableDiskLocked(); err != nil {
		c.mutableDiskDisabled = true
		c.warn(err)
	}
}

func (c *Cache) Generation() uint64 { c.mu.Lock(); defer c.mu.Unlock(); return c.generation }

// SeedDisk writes a validated background publication snapshot to disk without
// admitting it to memory or renewing any reader's idle timestamp.
func (c *Cache) SeedDisk(key string, p Policy, data []byte, etag string, generation uint64) {
	if key == "" {
		return
	}
	c.writeDisk(c.identity(key, p), p.Mutable, &record{data: bytes.Clone(data), etag: etag, generation: generation})
}

func (c *Cache) identity(key string, p Policy) string {
	if p.Mutable {
		return "mutable:" + key
	}
	return "immutable:" + key
}
func (c *Cache) removeLocked(element *list.Element) {
	old := element.Value.(*item)
	c.used -= old.weight
	delete(c.items, old.key)
	c.lru.Remove(element)
}
func (c *Cache) sweepLocked() {
	now := c.config.Now()
	for element := c.lru.Back(); element != nil; element = c.lru.Back() {
		if now.Before(element.Value.(*item).record.accessed.Add(c.config.TTL)) {
			break
		}
		c.removeLocked(element)
	}
}
func (c *Cache) resetTimerLocked() {
	if c.closed {
		return
	}
	oldest := c.lru.Back()
	if oldest == nil {
		if c.timer != nil {
			c.timer.Stop()
		}
		c.nextExpiry = time.Time{}
		return
	}
	expiry := oldest.Value.(*item).record.accessed.Add(c.config.TTL)
	if expiry.Equal(c.nextExpiry) {
		return
	}
	c.nextExpiry = expiry
	delay := max(time.Millisecond, expiry.Sub(c.config.Now()))
	if c.timer == nil {
		c.timer = time.AfterFunc(delay, func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			c.nextExpiry = time.Time{}
			c.sweepLocked()
			c.resetTimerLocked()
		})
	} else {
		c.timer.Reset(delay)
	}
}

// Sweep performs only local idle eviction. It never polls origin storage.
func (c *Cache) Sweep() { c.mu.Lock(); defer c.mu.Unlock(); c.sweepLocked(); c.resetTimerLocked() }
func (c *Cache) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.timer != nil {
		c.timer.Stop()
	}
}

func (c *Cache) Get(ctx context.Context, key string, p Policy) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if key == "" || p.Load == nil || p.Decode == nil {
		return nil, errors.New("metadata cache needs a key, loader and decoder")
	}
	identity := c.identity(key, p)
	c.mu.Lock()
	generation := c.generation
	c.sweepLocked()
	if element := c.items[identity]; element != nil {
		c.lru.MoveToFront(element)
		r := element.Value.(*item).record
		r.accessed = c.config.Now()
		c.resetTimerLocked()
		value, err := r.value, r.err
		c.mu.Unlock()
		return value, err
	}
	c.resetTimerLocked()
	flightKey := identity
	if p.Mutable {
		flightKey += "\x00" + strconv.FormatUint(generation, 10)
	}
	f := c.flights[flightKey]
	if f == nil {
		f = &flight{done: make(chan struct{})}
		c.flights[flightKey] = f
		// A disconnected first waiter must not cancel all other burst requests.
		loadCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), loadTimeout)
		go func() {
			defer cancel()
			r, err := c.read(loadCtx, identity, p, generation)
			if err == nil {
				f.value, f.err = r.value, r.err
			} else {
				f.err = err
			}
			c.mu.Lock()
			delete(c.flights, flightKey)
			close(f.done)
			c.mu.Unlock()
		}()
	}
	c.mu.Unlock()
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-f.done:
		return f.value, f.err
	}
}

func (c *Cache) read(ctx context.Context, key string, p Policy, generation uint64) (*record, error) {
	data, etag, ok := c.readDisk(key, p.Mutable)
	if ok {
		value, err := p.Decode(data)
		if err == nil {
			r := &record{data: data, etag: etag, value: value, accessed: c.config.Now(), generation: generation}
			c.retain(key, p, r)
			return r, nil
		}
		c.removeDisk(key, p.Mutable, generation)
	}
	data, etag, err := p.Load(ctx)
	if err != nil {
		return c.failure(key, p, generation, err)
	}
	data = bytes.Clone(data)
	value, err := p.Decode(data)
	if err != nil {
		return nil, err
	}
	r := &record{data: data, etag: etag, value: value, accessed: c.config.Now(), generation: generation}
	c.retain(key, p, r)
	c.writeDisk(key, p.Mutable, r)
	return r, nil
}

func (c *Cache) failure(key string, p Policy, generation uint64, err error) (*record, error) {
	if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	c.removeDisk(key, p.Mutable, generation)
	if !p.Mutable {
		return nil, err
	}
	r := &record{err: err, accessed: c.config.Now(), generation: generation}
	c.retain(key, p, r)
	return r, nil
}

func entryWeight(key string, r *record) int64 {
	return int64(len(r.data))*32 + int64(len(key)+len(r.etag))*2 + 4096
}
func (c *Cache) retain(key string, p Policy, r *record) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p.Mutable && r.generation != c.generation {
		return
	}
	weight := entryWeight(key, r)
	if element := c.items[key]; element != nil {
		c.used -= element.Value.(*item).weight
		c.lru.Remove(element)
		delete(c.items, key)
	}
	if weight > c.config.MemoryBytes {
		return
	}
	c.sweepLocked()
	for c.used+weight > c.config.MemoryBytes {
		element := c.lru.Back()
		old := element.Value.(*item)
		c.used -= old.weight
		delete(c.items, old.key)
		c.lru.Remove(element)
	}
	r.accessed = c.config.Now()
	element := c.lru.PushFront(&item{key: key, record: r, weight: weight})
	c.items[key] = element
	c.used += weight
	c.resetTimerLocked()
}
func (c *Cache) warn(err error) {
	c.warnOnce.Do(func() {
		slog.Warn("local metadata cache unavailable; using memory and Storage", slog.Any("err", fmt.Errorf("metadata cache: %w", err)))
	})
}
