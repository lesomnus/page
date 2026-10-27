package serving

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

type refreshStore struct {
	storage.Store
	mu               sync.Mutex
	gets             map[string]int
	lists, heads     int
	failKeys         map[string]bool
	pageLimit        int
	missingETag      bool
	fixedModified    bool
	afterList        func()
	afterGet         func(string)
	failContinuation atomic.Bool
	failLists        atomic.Bool
	failedLists      atomic.Int64
}

func (s *refreshStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.Info, error) {
	s.mu.Lock()
	if s.gets == nil {
		s.gets = map[string]int{}
	}
	s.gets[key]++
	fail, hook := s.failKeys[key], s.afterGet
	s.mu.Unlock()
	if fail {
		return nil, storage.Info{}, errors.New("injected metadata GET failure")
	}
	body, info, err := s.Store.Get(ctx, key)
	if s.missingETag {
		info.ETag = ""
	}
	if s.fixedModified {
		info.Modified = time.Unix(1000, 0)
	}
	if hook != nil {
		hook(key)
	}
	return body, info, err
}

func (s *refreshStore) Stat(ctx context.Context, key string) (storage.Info, error) {
	s.mu.Lock()
	s.heads++
	s.mu.Unlock()
	return s.Store.Stat(ctx, key)
}

func (s *refreshStore) List(ctx context.Context, prefix, cursor string, limit int) (storage.Page, error) {
	s.mu.Lock()
	s.lists++
	hook := s.afterList
	s.afterList = nil
	s.mu.Unlock()
	if s.failLists.Load() || (cursor != "" && s.failContinuation.Load()) {
		s.failedLists.Add(1)
		return storage.Page{}, errors.New("injected metadata LIST failure")
	}
	if s.pageLimit > 0 {
		limit = min(limit, s.pageLimit)
	}
	page, err := s.Store.List(ctx, prefix, cursor, limit)
	for i := range page.Objects {
		if s.missingETag {
			page.Objects[i].ETag = ""
		}
		if s.fixedModified {
			page.Objects[i].Modified = time.Unix(1000, 0)
		}
	}
	if err == nil && hook != nil {
		hook()
	}
	return page, err
}

func (s *refreshStore) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gets, s.lists, s.heads = map[string]int{}, 0, 0
}

func (s *refreshStore) counts() (map[string]int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	gets := map[string]int{}
	for key, count := range s.gets {
		gets[key] = count
	}
	return gets, s.lists, s.heads
}

func (s *refreshStore) fail(key string, fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failKeys == nil {
		s.failKeys = map[string]bool{}
	}
	s.failKeys[key] = fail
}

func requireCalls(t *testing.T, store *refreshStore, listCount int, wanted map[string]int) {
	t.Helper()
	gets, lists, heads := store.counts()
	if lists != listCount || heads != 0 || len(gets) != len(wanted) {
		t.Fatalf("metadata calls: lists=%d heads=%d gets=%v; want lists=%d gets=%v", lists, heads, gets, listCount, wanted)
	}
	for key, count := range wanted {
		if gets[key] != count {
			t.Fatalf("GET %s: got %d, want %d (%v)", key, gets[key], count, gets)
		}
	}
}

func manifestKey(r *deploy.Release) string {
	return deploy.ReleasePrefix(r.Owner, r.Site, r.ID) + "manifest.json"
}

func rawSite(t *testing.T, store storage.Store, site *deploy.Site) {
	t.Helper()
	body, err := json.Marshal(site)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), deploy.SiteKey(site.Name), strings.NewReader(string(body)), int64(len(body)), "application/json"); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshReusesImmutableReleasesAndReadsOnlyChangedSite(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one", "pages.json": `{"schemaVersion":1}`})
	preview := deploy.AliasToken("branch", "dev")
	aliases := map[string]deploy.Alias{preview: {Kind: "branch", Name: "dev", Revision: "one", Release: first.ID}}
	save(t, c, first, aliases)
	bar := publish(t, c, "bar", "one", map[string]string{"index.html": "bar"})
	save(t, c, bar, nil)
	store.reset()
	h := handler(t, c, "")
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1, deploy.SiteKey("bar"): 1, manifestKey(first): 1, manifestKey(bar): 1})
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, nil)
	// Writer-side readers return separate mutable values. An uncommitted sync
	// edit must never alter the serving cache even when the Catalog is shared.
	uncommitted, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	uncommitted.Production = bar.ID
	uncommitted.Aliases[preview] = deploy.Alias{Kind: "branch", Name: "dev", Revision: "bad", Release: bar.ID}
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, nil)
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("uncommitted writer state leaked: %q", got)
	}

	second := publish(t, c, "foo", "two", map[string]string{"index.html": "two"})
	save(t, c, second, aliases)
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1, manifestKey(second): 1})
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "two" {
		t.Fatalf("new release not visible: %q", got)
	}
	// Switching back to a release held by a preview requires only the site GET.
	aliases[preview] = deploy.Alias{Kind: "branch", Name: "dev", Revision: "two", Release: second.ID}
	save(t, c, first, aliases)
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1})
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("cached rollback not visible: %q", got)
	}
}

func TestRefreshCachesLegacyPagesMetadata(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "legacy", map[string]string{"index.html": "legacy", "pages.json": `{"schemaVersion":1,"routing":{"cleanUrls":true}}`})
	r.Pages = nil // A release published before routing was embedded in manifests.
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put(context.Background(), manifestKey(r), strings.NewReader(string(body)), int64(len(body)), "application/json"); err != nil {
		t.Fatal(err)
	}
	save(t, c, r, nil)
	store.reset()
	h := handler(t, c, "")
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1, manifestKey(r): 1, deploy.FileKey(r, "pages.json"): 1})
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, nil)
}

func TestRefreshCachesGETFingerprintAcrossListGetRace(t *testing.T) {
	base := storage.NewMemory()
	store := &refreshStore{Store: base, fixedModified: true}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	second := publish(t, c, "foo", "two", map[string]string{"index.html": "two"})
	save(t, c, first, nil)
	old, _, err := c.GetSiteInfo(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	newSite := *old
	newSite.Production = second.ID
	// LIST sees the first object; GET reads its replacement. LastModified and
	// object length remain identical, as can happen with fast control changes.
	store.afterList = func() { rawSite(t, base, &newSite) }
	h := handler(t, c, "")
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "two" {
		t.Fatalf("replacement not read: %q", got)
	}
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, nil)
	// Restoring the originally listed bytes must not be mistaken for an
	// unchanged object because that earlier LIST ETag was never cached.
	rawSite(t, base, old)
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1, manifestKey(first): 1})
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("same-second change missed: %q", got)
	}
}

func TestRefreshWithoutETagReadsSiteButReusesRelease(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory(), missingETag: true}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1})
}

func TestRefreshFailureDoesNotAdvanceCaches(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	bar := publish(t, c, "bar", "one", map[string]string{"index.html": "bar-one"})
	foo := publish(t, c, "foo", "one", map[string]string{"index.html": "foo-one"})
	save(t, c, bar, nil)
	save(t, c, foo, nil)
	h := handler(t, c, "")
	bar2 := publish(t, c, "bar", "two", map[string]string{"index.html": "bar-two"})
	foo2 := publish(t, c, "foo", "two", map[string]string{"index.html": "foo-two"})
	save(t, c, bar2, nil)
	save(t, c, foo2, nil)
	store.fail(manifestKey(foo2), true)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("accepted incomplete refresh")
	}
	if got := request(h, "GET", "bar.example.com", "/", nil).Body.String(); got != "bar-one" {
		t.Fatalf("partially advanced snapshot: %q", got)
	}
	store.fail(manifestKey(foo2), false)
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("bar"): 1, deploy.SiteKey("foo"): 1, manifestKey(bar2): 1, manifestKey(foo2): 1})
	if got := request(h, "GET", "bar.example.com", "/", nil).Body.String(); got != "bar-two" {
		t.Fatalf("recovery missed change: %q", got)
	}
}

func TestRefreshCompletesPaginationBeforePruning(t *testing.T) {
	base := storage.NewMemory()
	store := &refreshStore{Store: base, pageLimit: 1}
	c := deploy.New(store)
	foo := publish(t, c, "foo", "one", map[string]string{"index.html": "foo"})
	bar := publish(t, c, "bar", "one", map[string]string{"index.html": "bar"})
	save(t, c, foo, nil)
	save(t, c, bar, nil)
	if _, err := base.Put(context.Background(), "sites/aaa/control.json", strings.NewReader("{}"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}
	h := handler(t, c, "")
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 3, nil)
	if err := base.Delete(context.Background(), deploy.SiteKey("foo")); err != nil {
		t.Fatal(err)
	}
	store.failContinuation.Store(true)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("accepted partial inventory")
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != http.StatusOK {
		t.Fatalf("partial inventory revoked site: %d", got)
	}
	store.failContinuation.Store(false)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != http.StatusNotFound {
		t.Fatalf("completed inventory did not revoke site: %d", got)
	}
	if len(h.releases) != 1 || len(h.sites) != 1 {
		t.Fatalf("removed site retained in cache: %d releases, %d sites", len(h.releases), len(h.sites))
	}
	// Reintroducing a previously evicted release requires a fresh manifest read.
	save(t, c, foo, nil)
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 3, map[string]int{deploy.SiteKey("foo"): 1, manifestKey(foo): 1})
}

func TestRefreshValidatesOwnershipAndRevisionOnCacheHit(t *testing.T) {
	base := storage.NewMemory()
	c := deploy.New(base)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	site, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	site.RepositoryID = "another-repository"
	rawSite(t, base, site)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("cache bypassed repository ownership validation")
	}
	site.RepositoryID = r.Origin.RepositoryID
	site.Aliases[deploy.AliasToken("branch", "dev")] = deploy.Alias{Kind: "branch", Name: "dev", Revision: "wrong", Release: r.ID}
	rawSite(t, base, site)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("cache bypassed preview revision validation")
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("invalid metadata replaced snapshot: %q", got)
	}
}

func TestRefreshExpiresAssetCacheWithoutMetadataChange(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	old := publish(t, c, "foo", "old", map[string]string{"index.html": "old", "pages.json": `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`, "assets/old.js": "old-js"})
	save(t, c, old, nil)
	next := publish(t, c, "foo", "new", map[string]string{"index.html": "new"})
	save(t, c, next, nil)
	site, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	// SaveSite's asset retention comes from source config; install a lease here
	// to make time passage independent of writes and wall-clock test duration.
	until := time.Now().Add(time.Hour)
	site.AssetReleases = map[string]time.Time{old.ID: until}
	rawSite(t, store.Store, site)
	h := handler(t, c, "")
	if got := request(h, "GET", "foo.example.com", "/assets/old.js", nil).Code; got != http.StatusOK {
		t.Fatalf("lease missing before expiry: %d", got)
	}
	h.now = func() time.Time { return until.Add(time.Second) }
	if got := request(h, "GET", "foo.example.com", "/assets/old.js", nil).Code; got != http.StatusNotFound {
		t.Fatalf("lease expiry required a refresh: %d", got)
	}
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, nil)
	if len(h.releases) != 1 {
		t.Fatalf("expired assets retained in cache: %d releases", len(h.releases))
	}
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for notification/reconciliation")
		}
		time.Sleep(time.Millisecond)
	}
}

func runHandler(t *testing.T, c *deploy.Catalog, interval time.Duration) *Handler {
	return runHandlerWithRetry(t, c, interval, time.Second)
}

func runHandlerWithRetry(t *testing.T, c *deploy.Catalog, interval, retryInitial time.Duration) *Handler {
	t.Helper()
	h, err := New(c, Config{BaseDomain: "example.com", RefreshInterval: interval})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.run(ctx, retryInitial) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Run exit: %v", err)
		}
	})
	waitFor(t, func() bool { return h.current.Load() != nil })
	return h
}

func TestRunBoundsFastRetriesUntilAnotherPublication(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, first, nil)
	h := runHandlerWithRetry(t, c, time.Hour, time.Millisecond)
	second := publish(t, c, "foo", "two", map[string]string{"index.html": "two"})
	store.failLists.Store(true)
	save(t, c, second, nil)
	waitFor(t, func() bool { return store.failedLists.Load() >= 4 })
	time.Sleep(20 * time.Millisecond)
	if got := store.failedLists.Load(); got != 4 {
		t.Fatalf("unbounded retries after one failed notification: %d attempts", got)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("retry exhaustion discarded prior snapshot: %q", got)
	}
	// Recovery alone does not restart a hot retry loop. A new publication must
	// replenish the budget and wake the reader before its periodic hour tick.
	store.failLists.Store(false)
	time.Sleep(10 * time.Millisecond)
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("exhausted loop woke without an event: %q", got)
	}
	save(t, c, second, nil)
	waitFor(t, func() bool { return request(h, "GET", "foo.example.com", "/", nil).Body.String() == "two" })
}

func TestRunRefreshesOnPublicationAndRetriesLostAttempt(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, first, nil)
	h := runHandler(t, c, time.Hour)
	second := publish(t, c, "foo", "two", map[string]string{"index.html": "two"})
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("unpublished release activated: %q", got)
	}
	save(t, c, second, nil)
	waitFor(t, func() bool { return request(h, "GET", "foo.example.com", "/", nil).Body.String() == "two" })
	third := publish(t, c, "foo", "three", map[string]string{"index.html": "three"})
	store.failLists.Store(true)
	save(t, c, third, nil)
	waitFor(t, func() bool { return store.failedLists.Load() > 0 })
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "two" {
		t.Fatalf("failed notification discarded current snapshot: %q", got)
	}
	// No further publication signal: retry must recover before the hour ticker.
	store.failLists.Store(false)
	waitFor(t, func() bool { return request(h, "GET", "foo.example.com", "/", nil).Body.String() == "three" })
}

func TestRunPeriodicReconciliationFindsOtherProcessWrites(t *testing.T) {
	base := storage.NewMemory()
	c := deploy.New(base)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, first, nil)
	h := runHandler(t, c, 10*time.Millisecond)
	otherProcess := deploy.New(base)
	second := publish(t, otherProcess, "foo", "two", map[string]string{"index.html": "two"})
	save(t, otherProcess, second, nil)
	waitFor(t, func() bool { return request(h, "GET", "foo.example.com", "/", nil).Body.String() == "two" })
}

func TestRunKeepsNotificationDuringStartupRefresh(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	second := publish(t, c, "foo", "two", map[string]string{"index.html": "two"})
	save(t, c, first, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store.afterGet = func(key string) {
		if key == manifestKey(first) {
			once.Do(func() { close(entered); <-release })
		}
	}
	h, err := New(c, Config{BaseDomain: "example.com", RefreshInterval: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.Run(ctx) }()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		close(release)
		cancel()
		<-done
		t.Fatal("startup refresh did not reach old manifest")
	}
	// SaveSite itself rereads old manifests. Let those calls proceed while the
	// startup read stays blocked, without depending on goroutine scheduling.
	store.mu.Lock()
	store.afterGet = nil
	store.mu.Unlock()
	save(t, c, second, nil)
	close(release)
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return request(h, "GET", "foo.example.com", "/", nil).Body.String() == "two" })
}

func TestDefaultReconciliationInterval(t *testing.T) {
	h, err := New(deploy.New(storage.NewMemory()), Config{BaseDomain: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if h.config.RefreshInterval != 5*time.Minute {
		t.Fatalf("default refresh interval: %s", h.config.RefreshInterval)
	}
}
