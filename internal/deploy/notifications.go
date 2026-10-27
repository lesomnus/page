package deploy

import (
	"strings"
	"sync"
)

type changeKind uint8

const (
	publicationChanged changeKind = iota
	controlChanged
)

// changeHub supplies process-local wakeups. Storage remains authoritative;
// listeners reconcile periodically to discover writes from other processes.
type changeHub struct {
	mu       sync.Mutex
	watchers map[changeKind]map[chan struct{}]struct{}
}

// WatchPublications signals successful site.json writes through this Catalog.
// Signals coalesce: each listener must read current state, not count messages.
// The returned function unregisters the listener without closing its channel.
func (c *Catalog) WatchPublications() (<-chan struct{}, func()) {
	return c.changes.watch(publicationChanged)
}

// WatchControls wakes source owners after successful control.json writes through
// this Catalog. It does not apply operator intent or bypass the owning sync.
func (c *Catalog) WatchControls() (<-chan struct{}, func()) {
	return c.changes.watch(controlChanged)
}

func (h *changeHub) watch(kind changeKind) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.watchers == nil {
		h.watchers = make(map[changeKind]map[chan struct{}]struct{})
	}
	if h.watchers[kind] == nil {
		h.watchers[kind] = make(map[chan struct{}]struct{})
	}
	h.watchers[kind][ch] = struct{}{}
	h.mu.Unlock()
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			h.mu.Lock()
			delete(h.watchers[kind], ch)
			h.mu.Unlock()
		})
	}
}

func (c *Catalog) notifyWrite(key string) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 || parts[0] != "sites" || !ValidName(parts[1]) {
		return
	}
	var kind changeKind
	switch parts[2] {
	case "site.json":
		kind = publicationChanged
	case "control.json":
		kind = controlChanged
	default:
		return
	}
	c.changes.mu.Lock()
	defer c.changes.mu.Unlock()
	for ch := range c.changes.watchers[kind] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
