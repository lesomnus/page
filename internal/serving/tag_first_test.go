package serving

import (
	"context"
	"strings"
	"testing"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/metacache"
	"github.com/lesomnus/page/internal/storage"
	"time"
)

func TestTagFirstHTTPFallbackAndQualifiedCompatibility(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	prod := publish(t, c, "app", "production", map[string]string{"index.html": "production"})
	branch := publish(t, c, "app", "branch", map[string]string{"index.html": "branch"})
	tag := publish(t, c, "app", "tag", map[string]string{"index.html": "tag"})
	commit := publish(t, c, "app", "commit", map[string]string{"index.html": "commit"})
	b, tagToken, cm := deploy.AliasToken("branch", "refs/heads/release"), deploy.AliasToken("tag", "refs/tags/release"), deploy.AliasToken("commit", "commit")
	aliases := map[string]deploy.Alias{
		b:        {Kind: "branch", Name: "refs/heads/release", Revision: "branch", Release: branch.ID},
		tagToken: {Kind: "tag", Name: "refs/tags/release", Revision: "tag", Release: tag.ID},
		cm:       {Kind: "commit", Name: "commit", Revision: "commit", Release: commit.ID},
	}
	save(t, c, prod, aliases)
	for _, style := range []string{"flat", "nested"} {
		h, err := New(c, Config{BaseDomain: "example.com", PreviewStyle: style, PreviewAliasMode: "tag-first"})
		if err != nil {
			t.Fatal(err)
		}
		if err := h.Refresh(context.Background()); err != nil {
			t.Fatal(err)
		}
		short := deploy.Host("example.com", style, "app", "release")
		for host, body := range map[string]string{
			"app.example.com": "production", short: "tag",
			deploy.Host("example.com", style, "app", b):        "branch",
			deploy.Host("example.com", style, "app", tagToken): "tag",
			deploy.Host("example.com", style, "app", cm):       "commit",
		} {
			if got := request(h, "GET", host, "/", nil); got.Code != 200 || got.Body.String() != body {
				t.Fatalf("%s: %d %q", host, got.Code, got.Body.String())
			}
		}
		delete(aliases, tagToken)
		save(t, c, prod, aliases)
		if got := request(h, "GET", short, "/", nil); got.Code != 200 || got.Body.String() != "branch" {
			t.Fatalf("tag removal did not fall back without refresh: %d %q", got.Code, got.Body.String())
		}
		if got := request(h, "GET", deploy.Host("example.com", style, "app", tagToken), "/", nil).Code; got != 404 {
			t.Fatalf("removed qualified tag still served: %d", got)
		}
		aliases[tagToken] = deploy.Alias{Kind: "tag", Name: "refs/tags/release", Revision: "tag", Release: tag.ID}
		save(t, c, prod, aliases)
	}
}

func TestTagFirstAmbiguousShortNamesDoNotBlockProduction(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	prod := publish(t, c, "app", "prod", map[string]string{"index.html": "production"})
	branch := publish(t, c, "app", "branch", map[string]string{"index.html": "branch"})
	tag := publish(t, c, "app", "tag", map[string]string{"index.html": "tag"})
	aliases := map[string]deploy.Alias{}
	for _, name := range []string{"refs/tags/v1.0", "refs/tags/v1-0"} {
		aliases[deploy.AliasToken("tag", name)] = deploy.Alias{Kind: "tag", Name: name, Revision: "tag", Release: tag.ID}
	}
	aliases[deploy.AliasToken("branch", "refs/heads/v1-0")] = deploy.Alias{Kind: "branch", Name: "refs/heads/v1-0", Revision: "branch", Release: branch.ID}
	aliases[deploy.AliasToken("branch", strings.Repeat("long", 30))] = deploy.Alias{Kind: "branch", Name: strings.Repeat("long", 30), Revision: "branch", Release: branch.ID}
	save(t, c, prod, aliases)
	h, err := New(c, Config{BaseDomain: "example.com", PreviewAliasMode: "tag-first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := request(h, "GET", "v1-0--app.example.com", "/", nil); got.Code != 404 {
		t.Fatalf("ambiguous tag fell back to a branch: %d", got.Code)
	}
	if got := request(h, "GET", "app.example.com", "/", nil); got.Code != 200 || got.Body.String() != "production" {
		t.Fatal("production was affected by a bad short preview name")
	}
	for token, alias := range aliases {
		if got := request(h, "GET", deploy.Host("example.com", "flat", "app", token), "/", nil); got.Code != 200 || got.Body.String() != alias.Revision {
			t.Fatalf("qualified preview stopped working: %d %q", got.Code, got.Body.String())
		}
	}
}

func TestTagFirstCannotReplaceProductionOrQualifiedRoutes(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	prod := publish(t, c, "app", "production", map[string]string{"index.html": "app"})
	branch := publish(t, c, "app", "branch", map[string]string{"index.html": "branch"})
	tag := publish(t, c, "app", "tag", map[string]string{"index.html": "tag"})
	branchToken := deploy.AliasToken("branch", "refs/heads/dev")
	tagName := "refs/tags/" + branchToken
	aliases := map[string]deploy.Alias{
		branchToken:                       {Kind: "branch", Name: "refs/heads/dev", Revision: "branch", Release: branch.ID},
		deploy.AliasToken("tag", tagName): {Kind: "tag", Name: tagName, Revision: "tag", Release: tag.ID},
	}
	save(t, c, prod, aliases)
	other := publish(t, c, "dev--app", "other", map[string]string{"index.html": "other production"})
	save(t, c, other, nil)
	h, err := New(c, Config{BaseDomain: "example.com", PreviewAliasMode: "tag-first"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	for host, body := range map[string]string{
		"app.example.com":      "app",
		"dev--app.example.com": "other production",
		deploy.Host("example.com", "flat", "app", branchToken):                       "branch",
		deploy.Host("example.com", "flat", "app", deploy.AliasToken("tag", tagName)): "tag",
	} {
		if got := request(h, "GET", host, "/", nil); got.Code != 200 || got.Body.String() != body {
			t.Fatalf("reserved route %s changed: %d %q", host, got.Code, got.Body.String())
		}
	}
}

func TestFreshProductionReservationAfterFailedRefresh(t *testing.T) {
	base := storage.NewMemory()
	store := &refreshStore{Store: base}
	c := deploy.New(store)
	prod := publish(t, c, "bar--foo", "production", map[string]string{"index.html": "production"})
	branch := publish(t, c, "bar--foo", "branch", map[string]string{"index.html": "branch"})
	tag := publish(t, c, "bar--foo", "tag", map[string]string{"index.html": "tag"})
	branchToken := deploy.AliasToken("branch", "refs/heads/app")
	tagToken := deploy.AliasToken("tag", "refs/tags/app")
	aliases := map[string]deploy.Alias{
		branchToken: {Kind: "branch", Name: "refs/heads/app", Revision: "branch", Release: branch.ID},
		tagToken:    {Kind: "tag", Name: "refs/tags/app", Revision: "tag", Release: tag.ID},
	}
	save(t, c, prod, aliases)
	clock := newCacheClock()
	h, err := New(c, Config{BaseDomain: "example.com", PreviewAliasMode: "tag-first", MetadataCache: metacache.Config{Directory: t.TempDir(), Now: clock.now, TTL: 5 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	const host = "app--bar--foo.example.com"
	if got := request(h, "GET", host, "/", nil); got.Code != 200 || got.Body.String() != "tag" {
		t.Fatalf("initial tag preview: %d %q", got.Code, got.Body.String())
	}
	delete(aliases, tagToken)
	save(t, c, prod, aliases)
	store.failLists.Store(true)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected namespace refresh failure")
	}
	if got := request(h, "GET", host, "/", nil); got.Code != 200 || got.Body.String() != "branch" {
		t.Fatalf("fresh branch fallback after failed refresh: %d %q", got.Code, got.Body.String())
	}
	reserved := publish(t, c, "app--bar--foo", "reserved", map[string]string{"index.html": "reserved"})
	save(t, c, reserved, nil)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected namespace refresh failure after new site")
	}
	store.reset()
	if got := request(h, "GET", host, "/", nil).Code; got != 404 {
		t.Fatalf("existing short preview ignored new production reservation: %d", got)
	}
	gets, lists, _ := store.counts()
	store.mu.Lock()
	selectedHeads := store.stats[deploy.SiteKey("bar--foo")]
	store.mu.Unlock()
	if lists != 0 || gets[deploy.SiteKey("bar--foo")] != 1 || selectedHeads != 0 || gets[deploy.SiteKey("foo")] != 1 || gets[deploy.SiteKey("app--bar--foo")] != 1 || len(gets) != 3 {
		t.Fatalf("preview read unrelated sites or missed candidate: LIST=%d GET=%v selectedHEAD=%d", lists, gets, selectedHeads)
	}
	store.fail(deploy.SiteKey(reserved.Site), true)
	clock.advance(6 * time.Second)
	if got := request(h, "GET", host, "/", nil).Code; got != 404 {
		t.Fatalf("idle disk cache polled candidate origin: %d", got)
	}
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected namespace LIST failure")
	}
	if got := request(h, "GET", host, "/", nil).Code; got != 503 {
		t.Fatalf("candidate metadata failure became stale winner: %d", got)
	}
	store.fail(deploy.SiteKey(reserved.Site), false)
	site, err := c.GetSite(context.Background(), reserved.Site)
	if err != nil {
		t.Fatal(err)
	}
	for _, disabled := range []bool{false, true} {
		site.Production = ""
		site.Disabled = disabled
		rawSite(t, base, site)
		if got := request(h, "GET", host, "/", nil).Code; got != 404 {
			t.Fatalf("new site disabled=%v failed to reserve production hostname: %d", disabled, got)
		}
	}
	if err := base.Delete(context.Background(), deploy.SiteKey(reserved.Site)); err != nil {
		t.Fatal(err)
	}
	clock.advance(6 * time.Second)
	if err := h.Refresh(context.Background()); err == nil {
		t.Fatal("expected namespace LIST failure")
	}
	if got := request(h, "GET", host, "/", nil); got.Code != 200 || got.Body.String() != "branch" {
		t.Fatalf("missing candidate did not permit branch fallback: %d %q", got.Code, got.Body.String())
	}
}
