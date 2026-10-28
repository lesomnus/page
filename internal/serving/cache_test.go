package serving

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/routes"
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

func TestWarmRequestsAlwaysReadStorageMetadata(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	inputs := map[string]string{"index.html": "one"}
	for i := range 1000 {
		inputs[fmt.Sprintf("unused/%04d.txt", i)] = "unused"
	}
	r := publish(t, c, "foo", "one", inputs)
	save(t, c, r, nil)
	h := handler(t, c, "")
	for range 2 {
		store.reset()
		if got := request(h, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Body.String() != "one" {
			t.Fatalf("request: %d %q", got.Code, got.Body.String())
		}
		gets, lists, heads := store.counts()
		if lists != 0 || heads < 2 || gets[deploy.SiteKey("foo")] != 1 || gets[manifestKey(r)] != 0 {
			t.Fatalf("request metadata calls: GET=%v LIST=%d HEAD=%d", gets, lists, heads)
		}
		if len(gets) != 3 {
			t.Fatalf("request should read site, compact release and candidate file only: %v", gets)
		}
	}
}

func TestMetadataFailuresNeverServeStaleConfiguration(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	save(t, c, r, nil)
	h := handler(t, c, "")
	store.reset()
	if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 200 {
		t.Fatal(got)
	}
	gets, _, _ := store.counts()
	for key := range gets {
		t.Run(key, func(t *testing.T) {
			store.fail(key, true)
			if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != http.StatusServiceUnavailable {
				t.Fatalf("metadata failure used stale values: %d", got)
			}
			store.fail(key, false)
		})
	}
}

func TestFreshSiteChangesRevokeWithoutNamespaceRefresh(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	first := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	token := deploy.AliasToken("branch", "dev")
	save(t, c, first, map[string]deploy.Alias{token: {Kind: "branch", Name: "dev", Revision: "one", Release: first.ID}})
	h := handler(t, c, "")
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

func TestLegacyHTTPReadsMetadataOnEveryRequest(t *testing.T) {
	base := storage.NewMemory()
	store := &refreshStore{Store: base}
	c := deploy.New(store)
	r := publish(t, c, "foo", "legacy", map[string]string{"index.html": "legacy", "about.html": "about", "pages.json": `{"schemaVersion":1,"routing":{"cleanUrls":true}}`})
	r.Pages = nil
	body, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Put(context.Background(), manifestKey(r), strings.NewReader(string(body)), int64(len(body)), "application/json"); err != nil {
		t.Fatal(err)
	}
	if err := base.Delete(context.Background(), deploy.ReleasePrefix(r.Owner, r.Site, r.ID)+"serving.json"); err != nil {
		t.Fatal(err)
	}
	save(t, c, r, nil)
	h := handler(t, c, "")
	for range 2 {
		store.reset()
		if got := request(h, "GET", "foo.example.com", "/about", nil); got.Code != 200 || got.Body.String() != "about" {
			t.Fatalf("legacy clean URL: %d %q", got.Code, got.Body.String())
		}
		gets, _, _ := store.counts()
		if gets[manifestKey(r)] < 2 || gets[deploy.FileKey(r, "pages.json")] < 2 {
			t.Fatalf("legacy metadata was cached: %v", gets)
		}
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
	if got := request(h, "GET", "foo.example.com", "/assets/app.js", nil).Code; got != 503 {
		t.Fatalf("old release metadata failure became not found/stale asset: %d", got)
	}
	h.now = func() time.Time { return until.Add(time.Second) }
	if got := request(h, "GET", "foo.example.com", "/assets/app.js", nil).Code; got != 404 {
		t.Fatalf("expired old release was still read: %d", got)
	}
}
