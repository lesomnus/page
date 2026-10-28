package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

type refreshStore struct {
	storage.Store
	mu               sync.Mutex
	gets             map[string]int
	stats            map[string]int
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
	if s.stats == nil {
		s.stats = map[string]int{}
	}
	s.stats[key]++
	fail := s.failKeys[key]
	s.mu.Unlock()
	if fail {
		return storage.Info{}, errors.New("injected metadata HEAD failure")
	}
	info, err := s.Store.Stat(ctx, key)
	if s.missingETag {
		info.ETag = ""
	}
	return info, err
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
	s.gets, s.stats, s.lists, s.heads = map[string]int{}, map[string]int{}, 0, 0
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

func TestRefreshReadsOnlyNamespaceMetadata(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	alias := deploy.AliasToken("branch", "dev")
	save(t, c, first, map[string]deploy.Alias{alias: {Kind: "branch", Name: "dev", Revision: "one", Release: first.ID}})
	bar := publish(t, c, "bar", "one", map[string]string{"index.html": "bar"})
	save(t, c, bar, nil)
	store.reset()
	h := handler(t, c, "")
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1, deploy.SiteKey("bar"): 1})
	store.reset()
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireCalls(t, store, 1, map[string]int{deploy.SiteKey("foo"): 1, deploy.SiteKey("bar"): 1})
	if len(h.current.Load().routes) != 3 {
		t.Fatal("hostname bindings lost")
	}
}

type cacheClock struct{ nanos atomic.Int64 }

func newCacheClock() *cacheClock              { c := &cacheClock{}; c.nanos.Store(time.Now().UnixNano()); return c }
func (c *cacheClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *cacheClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }
func handlerWithCache(t *testing.T, c *deploy.Catalog, config metacache.Config) *Handler {
	t.Helper()
	h, err := New(c, Config{BaseDomain: "example.com", MetadataCache: config})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestConcurrentRequestsShareSiteReleaseAndCompiledRoutes(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	inputs := map[string]string{"index.html": "one", "pages.json": `{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Test":"shared"}}]}`}
	for i := range 1000 {
		inputs[fmt.Sprintf("unused/%04d.txt", i)] = "unused"
	}
	r := publish(t, c, "foo", "one", inputs)
	save(t, c, r, nil)
	h := handler(t, c, "")
	store.reset()
	start := make(chan struct{})
	failures := make(chan string, 50)
	var workers sync.WaitGroup
	for range 50 {
		workers.Go(func() {
			<-start
			got := request(h, "GET", "foo.example.com", "/", nil)
			if got.Code != 200 || got.Body.String() != "one" || got.Header().Get("X-Test") != "shared" {
				failures <- fmt.Sprintf("%d %q", got.Code, got.Body.String())
			}
		})
	}
	close(start)
	workers.Wait()
	close(failures)
	for failure := range failures {
		t.Error(failure)
	}
	gets, lists, _ := store.counts()
	if lists != 0 || gets[deploy.SiteKey("foo")] != 1 || gets[manifestKey(r)] != 0 {
		t.Fatalf("burst repeated site or inventory reads: %v LIST%d", gets, lists)
	}
	rel, err := c.GetServingRelease(context.Background(), r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The explicit reader above accounts for the second descriptor GET.
	gets, _, _ = store.counts()
	if gets[deploy.ServingReleaseKey(rel)] != 2 || gets[deploy.ServingFileKey(rel, "index.html")] != 50 || len(gets) != 3 {
		t.Fatalf("burst did not share descriptor or cached file inventory: %v", gets)
	}
	site, err := h.cachedSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	first, err := h.cachedRelease(context.Background(), site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := h.cachedRelease(context.Background(), site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.engine != second.engine || first.release != second.release {
		t.Fatal("warm request recompiled routes")
	}
}

func TestExpiredSiteRevalidatesWithoutDeletingOrRepeatingGET(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	clock := newCacheClock()
	h := handlerWithCache(t, c, metacache.Config{Now: clock.now, TTL: 5 * time.Second})
	first, err := h.cachedSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	store.reset()
	clock.advance(6 * time.Second)
	second, err := h.cachedSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	gets, _, heads := store.counts()
	if first != second || len(gets) != 0 || heads != 1 {
		t.Fatalf("TTL discarded unchanged decoded metadata: same=%v GET%v HEAD%d", first == second, gets, heads)
	}
	// An external process changes Storage without local publication notifications.
	changed, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	changed.Disabled = true
	rawSite(t, store.Store, changed)
	store.reset()
	if got, err := h.cachedSite(context.Background(), "foo"); err != nil || got.Disabled {
		t.Fatal("cache ignored its freshness TTL")
	}
	clock.advance(6 * time.Second)
	got, err := h.cachedSite(context.Background(), "foo")
	if err != nil || !got.Disabled {
		t.Fatalf("changed ETag not reloaded: %v", err)
	}
	gets, _, heads = store.counts()
	if gets[deploy.SiteKey("foo")] != 1 || heads != 1 {
		t.Fatalf("changed metadata calls: GET%v HEAD%d", gets, heads)
	}
}

func TestFreshCacheSurvivesOriginFailureButExpiredSiteDoesNotServeStale(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	clock := newCacheClock()
	h := handlerWithCache(t, c, metacache.Config{Now: clock.now, TTL: 5 * time.Second})
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 200 {
		t.Fatal(got)
	}
	rel, err := c.GetServingRelease(context.Background(), r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.fail(deploy.SiteKey("foo"), true)
	store.fail(deploy.ServingReleaseKey(rel), true)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 200 {
		t.Fatalf("fresh validated cache consulted failed origin: %d", got)
	}
	clock.advance(6 * time.Second)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 503 {
		t.Fatalf("expired Site hid failed revalidation: %d", got)
	}
	store.fail(deploy.SiteKey("foo"), false)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 200 {
		t.Fatalf("unchanged immutable release was needlessly reloaded: %d", got)
	}
	store.fail(deploy.ServingFileKey(rel, "index.html"), true)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 503 {
		t.Fatalf("uncached file metadata failure swallowed: %d", got)
	}
}

func TestFreshSiteChangesRevokeWithoutNamespaceRefresh(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	token := deploy.AliasToken("branch", "dev")
	save(t, c, first, map[string]deploy.Alias{token: {Kind: "branch", Name: "dev", Revision: "one", Release: first.ID}})
	clock := newCacheClock()
	h := handlerWithCache(t, c, metacache.Config{Now: clock.now, TTL: 5 * time.Second})
	store.failLists.Store(true)
	second := publish(t, c, "foo", "two", map[string]string{"index.html": "two"})
	save(t, c, second, nil)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected LIST failure")
	}
	if got := request(h, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Body.String() != "two" {
		t.Fatalf("fresh release hidden behind old namespace: %d %q", got.Code, got.Body.String())
	}
	if got := request(h, "GET", deploy.Host("example.com", "flat", "foo", token), "/", nil).Code; got != 404 {
		t.Fatalf("removed alias still served: %d", got)
	}
	site, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	site.Disabled = true
	rawSite(t, store.Store, site)
	clock.advance(6 * time.Second)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("disabled site still served: %d", got)
	}
	if err := store.Delete(context.Background(), deploy.SiteKey("foo")); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("deleted site still served: %d", got)
	}
}

func TestRequestsValidateRepositoryAndPreviewRevision(t *testing.T) {
	store := storage.NewMemory()
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	token := deploy.AliasToken("branch", "dev")
	save(t, c, r, map[string]deploy.Alias{token: {Kind: "branch", Name: "dev", Revision: "one", Release: r.ID}})
	h := handler(t, c, "")
	site, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	site.RepositoryID = "another-repository"
	rawSite(t, store, site)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("new repository reused old binding: %d", got)
	}
	// Reconciliation can accept a new binding, but never a release owned by the
	// former repository. Production metadata is validated on every request.
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 503 {
		t.Fatalf("release repository mismatch: %d", got)
	}
	site.RepositoryID = r.Origin.RepositoryID
	site.Aliases[token] = deploy.Alias{Kind: "branch", Name: "dev", Revision: "wrong", Release: r.ID}
	rawSite(t, store, site)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", deploy.Host("example.com", "flat", "foo", token), "/", nil).Code; got != 503 {
		t.Fatalf("preview revision mismatch: %d", got)
	}
}

func TestRefreshCompletesPaginationBeforeReplacingNamespace(t *testing.T) {
	base := storage.NewMemory()
	store := &refreshStore{Store: base, pageLimit: 1}
	c := deploy.New(store)
	foo := publish(t, c, "foo", "one", map[string]string{"index.html": "foo"})
	bar := publish(t, c, "bar", "one", map[string]string{"index.html": "bar"})
	save(t, c, foo, nil)
	save(t, c, bar, nil)
	h := handler(t, c, "")
	before := h.current.Load()
	if err := base.Delete(context.Background(), deploy.SiteKey("foo")); err != nil {
		t.Fatal(err)
	}
	store.failContinuation.Store(true)
	// Ensure pagination still needs a second page after the deletion.
	if _, err := base.Put(context.Background(), "sites/aaa/control.json", strings.NewReader("{}"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("accepted partial inventory")
	}
	if h.current.Load() != before {
		t.Fatal("partial inventory replaced namespace")
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("deleted site served through retained binding: %d", got)
	}
	if got := request(h, "GET", "bar.example.com", "/", nil).Code; got != 200 {
		t.Fatalf("existing site lost: %d", got)
	}
	store.failContinuation.Store(false)
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(h.current.Load().routes) != 1 {
		t.Fatal("removed hostname retained after complete inventory")
	}
}

func TestSelectedObjectStatPreservesMissingAndSizeValidation(t *testing.T) {
	base := storage.NewMemory()
	c := deploy.New(base)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	key := deploy.FileKey(r, "index.html")
	if _, err := base.Put(context.Background(), key, strings.NewReader("different"), 9, "text/html"); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 503 {
		t.Fatalf("size mismatch: %d", got)
	}
	if err := base.Delete(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("missing selected object: %d", got)
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
	second := publish(t, c, "bar", "two", map[string]string{"index.html": "two"})
	store.failLists.Store(true)
	save(t, c, second, nil)
	waitFor(t, func() bool { return store.failedLists.Load() >= 4 })
	time.Sleep(20 * time.Millisecond)
	if got := store.failedLists.Load(); got != 4 {
		t.Fatalf("unbounded retries after one failed notification: %d attempts", got)
	}
	if got := request(h, "GET", "foo.example.com", "/", nil).Body.String(); got != "one" {
		t.Fatalf("retry exhaustion discarded namespace: %q", got)
	}
	store.failLists.Store(false)
	time.Sleep(10 * time.Millisecond)
	if got := request(h, "GET", "bar.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("exhausted loop woke without an event: %d", got)
	}
	save(t, c, second, nil)
	waitFor(t, func() bool { return request(h, "GET", "bar.example.com", "/", nil).Body.String() == "two" })
}

func TestRunRefreshesNamespaceOnPublicationAndRetriesLostAttempt(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, first, nil)
	h := runHandler(t, c, time.Hour)
	second := publish(t, c, "bar", "two", map[string]string{"index.html": "two"})
	if got := request(h, "GET", "bar.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("unpublished namespace activated: %d", got)
	}
	save(t, c, second, nil)
	waitFor(t, func() bool { return request(h, "GET", "bar.example.com", "/", nil).Body.String() == "two" })
	third := publish(t, c, "baz", "three", map[string]string{"index.html": "three"})
	store.failLists.Store(true)
	save(t, c, third, nil)
	waitFor(t, func() bool { return store.failedLists.Load() > 0 })
	if got := request(h, "GET", "bar.example.com", "/", nil).Body.String(); got != "two" {
		t.Fatalf("failed notification discarded namespace: %q", got)
	}
	store.failLists.Store(false)
	waitFor(t, func() bool { return request(h, "GET", "baz.example.com", "/", nil).Body.String() == "three" })
}

func TestRunPeriodicReconciliationFindsOtherProcessNamespaces(t *testing.T) {
	base := storage.NewMemory()
	c := deploy.New(base)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, first, nil)
	h := runHandler(t, c, 10*time.Millisecond)
	otherProcess := deploy.New(base)
	second := publish(t, otherProcess, "bar", "two", map[string]string{"index.html": "two"})
	save(t, otherProcess, second, nil)
	waitFor(t, func() bool { return request(h, "GET", "bar.example.com", "/", nil).Body.String() == "two" })
}

func TestRunKeepsNotificationDuringStartupRefresh(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	second := publish(t, c, "bar", "two", map[string]string{"index.html": "two"})
	save(t, c, first, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store.afterGet = func(key string) {
		if key == deploy.SiteKey("foo") {
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
		t.Fatal("startup refresh did not read site metadata")
	}
	store.mu.Lock()
	store.afterGet = nil
	store.mu.Unlock()
	save(t, c, second, nil)
	close(release)
	t.Cleanup(func() { cancel(); <-done })
	waitFor(t, func() bool { return request(h, "GET", "bar.example.com", "/", nil).Body.String() == "two" })
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

func TestCorruptServingMetadataReturnsUnavailable(t *testing.T) {
	base := storage.NewMemory()
	c := deploy.New(base)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	ctx := context.Background()
	rel, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := c.GetServingFile(ctx, rel, "index.html")
	if err != nil {
		t.Fatal(err)
	}
	put := func(key string, value any) {
		t.Helper()
		body, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := base.Put(ctx, key, strings.NewReader(string(body)), int64(len(body)), "application/json"); err != nil {
			t.Fatal(err)
		}
	}
	file.RepositoryID = "another-repository"
	put(deploy.ServingFileKey(rel, "index.html"), file)
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 503 {
		t.Fatalf("corrupt point metadata: %d", got)
	}
	file.RepositoryID = rel.Origin.RepositoryID
	put(deploy.ServingFileKey(rel, "index.html"), file)
	rel.Pages = &routes.Config{SchemaVersion: 1, Headers: []routes.HeaderRule{{From: "/*", Set: map[string]string{"Set-Cookie": "secret"}}}}
	put(deploy.ServingReleaseKey(rel), rel)
	// A cold reader must validate immutable routing from Storage. A warm reader
	// deliberately trusts the already completed immutable release identity.
	h = handler(t, c, "")
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 503 {
		t.Fatalf("unsafe routing metadata: %d", got)
	}
}

func TestDeclaredMissingBodyDoesNotBecomeSPAFallback(t *testing.T) {
	base := storage.NewMemory()
	c := deploy.New(base)
	r := publish(t, c, "foo", "one", map[string]string{
		"index.html":    "SPA",
		"assets/app.js": "app",
		"pages.json":    `{"schemaVersion":1,"routing":{"fallback":{"rewrite":"/index.html"}}}`,
	})
	save(t, c, r, nil)
	h := handler(t, c, "")
	if err := base.Delete(context.Background(), deploy.FileKey(r, "assets/app.js")); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "foo.example.com", "/assets/app.js", map[string]string{"Accept": "text/html"}); got.Code != 404 || strings.Contains(got.Body.String(), "SPA") {
		t.Fatalf("missing body became fallback: %d %q", got.Code, got.Body.String())
	}
}

func TestPreviousAssetsUseNewestLeaseAndPropagateMetadataFailure(t *testing.T) {
	base := storage.NewMemory()
	store := &refreshStore{Store: base}
	c := deploy.New(store)
	old := publish(t, c, "foo", "old", map[string]string{
		"assets/app.js": "old",
		"pages.json":    `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`,
	})
	newer := publish(t, c, "foo", "newer", map[string]string{
		"assets/app.js": "newer",
		"pages.json":    `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`,
	})
	current := publish(t, c, "foo", "current", map[string]string{"index.html": "current"})
	save(t, c, current, nil)
	site, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	until := time.Now().Add(time.Hour)
	// Install the leases directly so this test can exercise deterministic
	// selection even when two historical releases disagree on the same URL.
	// Ordinary publication forbids that immutable content collision.
	site.AssetReleases = map[string]time.Time{old.ID: until, newer.ID: until}
	rawSite(t, base, site)
	h := handler(t, c, "")
	for range 5 {
		if got := request(h, "GET", "foo.example.com", "/assets/app.js", nil); got.Code != 200 || got.Body.String() != "newer" {
			t.Fatalf("lease order depended on map iteration: %d %q", got.Code, got.Body.String())
		}
	}
	rel, err := c.GetServingRelease(context.Background(), newer.Owner, newer.Site, newer.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.fail(deploy.ServingFileKey(rel, "assets/app.js"), true)
	if got := request(h, "GET", "foo.example.com", "/assets/app.js", nil).Code; got != 503 {
		t.Fatalf("old file metadata failure became not found/stale asset: %d", got)
	}
	store.fail(deploy.ServingFileKey(rel, "assets/app.js"), false)
	store.fail(deploy.ServingReleaseKey(rel), true)
	if got := request(h, "GET", "foo.example.com", "/assets/app.js", nil).Code; got != 200 {
		t.Fatalf("cached immutable release reloaded unnecessarily: %d", got)
	}
	cold := handler(t, c, "")
	if got := request(cold, "GET", "foo.example.com", "/assets/app.js", nil).Code; got != 503 {
		t.Fatalf("cold old release metadata failure became not found/stale asset: %d", got)
	}
	h.now = func() time.Time { return until.Add(time.Second) }
	if got := request(h, "GET", "foo.example.com", "/assets/app.js", nil).Code; got != 404 {
		t.Fatalf("expired old release was still read: %d", got)
	}
}
