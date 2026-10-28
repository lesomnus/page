package serving

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/storage"
)

func TestDiskCacheReloadsCompiledReleaseAndRevalidatesMutableSite(t *testing.T) {
	ctx := context.Background()
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{
		"index.html": "one",
		"pages.json": `{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Disk":"reloaded"}}]}`,
	})
	save(t, c, r, nil)
	clock := newCacheClock()
	config := metacache.Config{Directory: t.TempDir(), Namespace: "one-origin", Now: clock.now}
	first := handlerWithCache(t, c, config)
	if got := request(first, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Header().Get("X-Disk") != "reloaded" {
		t.Fatalf("warm disk cache: %d %q", got.Code, got.Body.String())
	}
	if first.metadata.Stats().DiskBytes == 0 {
		t.Fatal("no metadata persisted to disk")
	}

	// A fresh Handler represents a restart with an empty memory tier. Only
	// the mutable Site needs an origin HEAD; the immutable descriptor and
	// its compiled routing are recovered from the local disk record.
	restarted := handlerWithCache(t, c, config)
	store.reset()
	store.fail(deploy.ReleasePrefix(r.Owner, r.Site, r.ID)+"serving.json", true)
	store.fail(manifestKey(r), true)
	if got := request(restarted, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Header().Get("X-Disk") != "reloaded" {
		t.Fatalf("disk cache needed unavailable immutable origin: %d %q", got.Code, got.Body.String())
	}
	gets, lists, heads := store.counts()
	if gets[deploy.SiteKey("foo")] != 0 || len(gets) != 1 || lists != 0 || heads != 2 {
		t.Fatalf("restart repeated immutable reads: GET%v LIST%d HEAD%d", gets, lists, heads)
	}

	// A second restart must not bless stale Site data merely because its disk
	// file exists and its original memory TTL has not elapsed.
	again := handlerWithCache(t, c, config)
	site, err := c.GetSite(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	site.Disabled = true
	rawSite(t, store.Store, site)
	store.reset()
	if got := request(again, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("disk promotion hid changed publication: %d", got)
	}
	gets, _, heads = store.counts()
	if gets[deploy.SiteKey("foo")] != 1 || heads != 1 {
		t.Fatalf("mutable disk record was not revalidated/reloaded: GET%v HEAD%d", gets, heads)
	}
}

func TestReleaseCapacityEvictionPromotesDiskWithoutOrigin(t *testing.T) {
	ctx := context.Background()
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	files := map[string]string{
		"index.html": "one",
		"pages.json": `{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Disk":"bounded metadata"}}]}`,
	}
	first := publish(t, c, "foo", "first", files)
	second := publish(t, c, "foo", "second", files)
	save(t, c, first, nil)
	site, err := c.GetSite(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	h := handlerWithCache(t, c, metacache.Config{Directory: t.TempDir(), MemoryBytes: 32 << 10})
	original, err := h.cachedRelease(ctx, site, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.cachedRelease(ctx, site, second.ID); err != nil {
		t.Fatal(err)
	}
	stats := h.metadata.Stats()
	if stats.Entries != 1 || stats.MemoryBytes > 32<<10 {
		t.Fatalf("small memory tier did not evict an old release: %+v", stats)
	}
	store.reset()
	store.fail(deploy.ReleasePrefix(first.Owner, first.Site, first.ID)+"serving.json", true)
	store.fail(manifestKey(first), true)
	reloaded, err := h.cachedRelease(ctx, site, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.engine == original.engine || reloaded.release.ID != original.release.ID {
		t.Fatal("evicted release was not rebuilt from its disk descriptor")
	}
	gets, lists, heads := store.counts()
	if len(gets) != 0 || lists != 0 || heads != 0 {
		t.Fatalf("disk promotion touched origin: GET%v LIST%d HEAD%d", gets, lists, heads)
	}
}

func TestMissingPreviewCandidateIsSharedUntilPublication(t *testing.T) {
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one"})
	token := deploy.AliasToken("branch", "refs/heads/dev")
	save(t, c, r, map[string]deploy.Alias{token: {Kind: "branch", Name: "refs/heads/dev", Revision: "one", Release: r.ID}})
	h, err := New(c, Config{BaseDomain: "example.com", PreviewAliasMode: "tag-first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	store.reset()
	for range 10 {
		if got := request(h, "GET", "dev--foo.example.com", "/", nil).Code; got != 200 {
			t.Fatal(got)
		}
	}
	gets, _, _ := store.counts()
	if gets[deploy.SiteKey("dev--foo")] != 1 {
		t.Fatalf("preview burst repeatedly queried nonexistent candidate: %v", gets)
	}
	reserved := publish(t, c, "dev--foo", "reserved", map[string]string{"index.html": "reserved"})
	save(t, c, reserved, nil)
	// Run and Refresh need not have consumed a notification for a new
	// production reservation to invalidate the negative candidate cache.
	if got := request(h, "GET", "dev--foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("publication failed to revoke cached negative reservation: %d", got)
	}
}

func TestCacheConfigRejectsNegativeBudgetsAndTTL(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	for _, cache := range []metacache.Config{{MemoryBytes: -1}, {DiskBytes: -1}, {TTL: -time.Second}} {
		if _, err := New(c, Config{BaseDomain: "example.com", MetadataCache: cache}); err == nil {
			t.Fatalf("accepted invalid cache configuration: %+v", cache)
		}
	}
}
