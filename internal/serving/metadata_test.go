package serving

import (
	"context"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/storage"
)

func TestDiskCacheReloadsCompiledReleaseAndRefreshSeedsMutableSite(t *testing.T) {
	ctx := context.Background()
	store := &refreshStore{Store: storage.NewMemory()}
	c := deploy.New(store)
	r := publish(t, c, "foo", "one", map[string]string{"index.html": "one", "pages.json": `{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Disk":"reloaded"}}]}`})
	save(t, c, r, nil)
	clock := newCacheClock()
	config := metacache.Config{Directory: t.TempDir(), Namespace: "one-origin", Now: clock.now, TTL: 5 * time.Second}
	first := handlerWithCache(t, c, config)
	if got := request(first, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Header().Get("X-Disk") != "reloaded" {
		t.Fatalf("warm disk cache: %d %q", got.Code, got.Body.String())
	}
	if first.metadata.Stats().DiskBytes == 0 {
		t.Fatal("no disk metadata cached")
	}
	// Startup refresh fetches mutable Site data once and seeds only the disk
	// tier. Requests recover both records without per-request origin checks.
	restarted := handlerWithCache(t, c, config)
	store.reset()
	store.fail(deploy.ReleasePrefix(r.Owner, r.Site, r.ID)+"serving.json", true)
	store.fail(manifestKey(r), true)
	if got := request(restarted, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Header().Get("X-Disk") != "reloaded" {
		t.Fatalf("disk reuse needed unavailable immutable origin: %d %q", got.Code, got.Body.String())
	}
	gets, lists, heads := store.counts()
	if len(gets) != 1 || gets[deploy.SiteKey("foo")] != 0 || lists != 0 || heads != 1 {
		t.Fatalf("disk request rechecked metadata: GET%v LIST%d HEAD%d", gets, lists, heads)
	}
	again := handlerWithCache(t, c, config)
	site, err := c.GetSite(ctx, "foo")
	if err != nil {
		t.Fatal(err)
	}
	site.Disabled = true
	rawSite(t, store.Store, site)
	store.reset()
	clock.advance(6 * time.Second)
	if got := request(again, "GET", "foo.example.com", "/", nil).Code; got != 200 {
		t.Fatalf("cache promotion polled an external publication: %d", got)
	}
	gets, _, heads = store.counts()
	if gets[deploy.SiteKey("foo")] != 0 || heads != 1 {
		t.Fatalf("disk promotion revalidated metadata: GET%v HEAD%d", gets, heads)
	}
	if err := again.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	store.reset()
	if got := request(again, "GET", "foo.example.com", "/", nil).Code; got != 404 {
		t.Fatalf("background refresh did not discover disabled Site: %d", got)
	}
	current, err := again.cachedSite(ctx, "foo")
	if err != nil || !current.Disabled {
		t.Fatalf("refresh did not replace mutable disk value: %v", err)
	}
	gets, _, heads = store.counts()
	if len(gets) != 0 || heads != 0 {
		t.Fatalf("fresh seeded metadata caused origin checks: GET%v HEAD%d", gets, heads)
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

func TestCollectedReleaseRepublishedWithSameIDDoesNotReuseOldMetadata(t *testing.T) {
	for _, restart := range []bool{false, true} {
		t.Run(map[bool]string{false: "memory", true: "disk-restart"}[restart], func(t *testing.T) {
			ctx := context.Background()
			c := deploy.New(storage.NewMemory())
			files := map[string]string{"index.html": "fixed revision content"}
			first := publish(t, c, "foo", "same-revision", files)
			save(t, c, first, nil)
			config := metacache.Config{Directory: t.TempDir(), Namespace: "republish-test"}
			h := handlerWithCache(t, c, config)
			if got := request(h, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Body.String() != files["index.html"] {
				t.Fatalf("initial publication: %d %q", got.Code, got.Body.String())
			}
			before, err := c.GetSite(ctx, "foo")
			if err != nil {
				t.Fatal(err)
			}
			withdrawn := *before
			withdrawn.Production = ""
			withdrawn.Aliases = nil
			if err := c.SaveSite(ctx, &withdrawn); err != nil {
				t.Fatal(err)
			}
			if got := request(h, "GET", "foo.example.com", "/", nil).Code; got != 404 {
				t.Fatalf("withdrawn publication still served: %d", got)
			}
			if err := c.DeleteRelease(ctx, &withdrawn, first.ID); err != nil {
				t.Fatal(err)
			}
			second := publish(t, c, "foo", "same-revision", files)
			if first.ID != second.ID || first.CreatedAt.Equal(second.CreatedAt) {
				t.Fatal("fixture did not recreate the same release ID with a new creation time")
			}
			save(t, c, second, nil)
			after, err := c.GetSite(ctx, "foo")
			if err != nil {
				t.Fatal(err)
			}
			if before.UpdatedAt.Equal(after.UpdatedAt) {
				t.Fatal("publication did not advance its metadata cache identity")
			}
			if restart {
				// The disk still contains the old completed descriptor. A fresh
				// process must select a different key using the current Site.
				h = handlerWithCache(t, c, config)
			}
			if got := request(h, "GET", "foo.example.com", "/", nil); got.Code != 200 || got.Body.String() != files["index.html"] {
				t.Fatalf("republished release reused old creation metadata: %d %q", got.Code, got.Body.String())
			}
			cached, err := h.cachedRelease(ctx, after, second.ID)
			if err != nil || !cached.release.CreatedAt.Equal(second.CreatedAt) {
				t.Fatalf("cached descriptor does not describe the new release incarnation: %v", err)
			}
		})
	}
}
