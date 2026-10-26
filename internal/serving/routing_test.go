package serving

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func TestPagesRoutingOverHTTP(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	config := `{"schemaVersion":1,"routing":{"cleanUrls":true,"routes":[{"from":"/old","redirect":"/about","status":308},{"from":"/guide","rewrite":"/about.html"},{"from":"/users/:name","rewrite":"/users/:name.html"}],"fallback":{"rewrite":"/index.html","exclude":["/assets/*"]},"errorPages":{"404":"/404.html"}},"headers":[{"from":"/*","set":{"X-Page":"base"}},{"from":"/users/*","set":{"X-Page":"users"}},{"from":"/guide","set":{"X-Route":"original"}},{"from":"/about.html","set":{"X-Route":"target"}}]}`
	r := publish(t, c, "foo", "routes", map[string]string{
		"pages.json": config, "index.html": "home", "about.html": "about", "404.html": "missing",
		"users/alice.html": "alice", "assets/app.js": "javascript", "docs/index.html": "docs",
	})
	save(t, c, r, nil)
	h := handler(t, c, "flat")
	for _, tc := range []struct {
		path, body string
		status     int
	}{
		{"/", "home", 200}, {"/about", "about", 200}, {"/guide", "about", 200},
		{"/users/alice", "alice", 200}, {"/spa/deep/link", "home", 200},
		{"/assets/missing.js", "missing", 404}, {"/users/missing", "missing", 404},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := request(h, "GET", "foo.example.com", tc.path, nil)
			if w.Code != tc.status || w.Body.String() != tc.body {
				t.Fatalf("got %d %q", w.Code, w.Body.String())
			}
		})
	}
	redirect := request(h, "GET", "foo.example.com", "/old?q=1", nil)
	if redirect.Code != 308 || redirect.Header().Get("Location") != "/about?q=1" || redirect.Header().Get("X-Page") != "base" {
		t.Fatalf("redirect: %d %v", redirect.Code, redirect.Header())
	}
	rewrite := request(h, "GET", "foo.example.com", "/guide", nil)
	if rewrite.Header().Get("X-Route") != "original" || rewrite.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("rewrite headers: %v", rewrite.Header())
	}
	users := request(h, "GET", "foo.example.com", "/users/alice", nil)
	if users.Header().Get("X-Page") != "users" {
		t.Fatalf("header order: %v", users.Header())
	}
	jsonRequest := request(h, "GET", "foo.example.com", "/missing-api", map[string]string{"Accept": "application/json"})
	if jsonRequest.Code != 404 || jsonRequest.Body.String() != "missing" {
		t.Fatalf("non-navigation used SPA fallback: %d %q", jsonRequest.Code, jsonRequest.Body.String())
	}
	head := request(h, "HEAD", "foo.example.com", "/assets/missing.js", map[string]string{"If-None-Match": "*", "Range": "bytes=0-1"})
	if head.Code != 404 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "7" || head.Header().Get("Content-Range") != "" {
		t.Fatalf("HEAD error document: %d %q %v", head.Code, head.Body.String(), head.Header())
	}
	errorBody := request(h, "GET", "foo.example.com", "/assets/missing.js", map[string]string{"If-None-Match": "*", "Range": "bytes=0-1"})
	if errorBody.Code != 404 || errorBody.Body.String() != "missing" || errorBody.Header().Get("Content-Range") != "" || errorBody.Header().Get("ETag") != "" {
		t.Fatalf("conditional/range changed error document: %d %q %v", errorBody.Code, errorBody.Body.String(), errorBody.Header())
	}
}

func TestPreviousProductionAssetGraceAndScope(t *testing.T) {
	c := deploy.New(storage.NewMemory())
	config := `{"schemaVersion":1,"routing":{"fallback":{"rewrite":"/index.html"}},"assets":{"immutable":["/assets/*"]}}`
	old := publish(t, c, "foo", "old-production", map[string]string{"pages.json": config, "index.html": "old HTML", "old.html": "old page", "assets/app.a1.js": "old javascript"})
	save(t, c, old, nil)
	preview := publish(t, c, "foo", "unpublished-preview", map[string]string{"pages.json": config, "index.html": "preview", "assets/secret.js": "not public"})
	_ = preview
	current := publish(t, c, "foo", "current-production", map[string]string{"pages.json": config, "index.html": "new HTML", "assets/app.b2.js": "new javascript"})
	site, err := c.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	site.Production = current.ID
	site.AssetRetention = time.Hour
	token := deploy.AliasToken("branch", "feature/current")
	site.Aliases = map[string]deploy.Alias{token: {Kind: "branch", Name: "feature/current", Revision: current.Origin.Revision, Release: current.ID}}
	if err = c.SaveSite(context.Background(), site); err != nil {
		t.Fatal(err)
	}
	h := handler(t, c, "flat")
	h.now = func() time.Time { return now }
	oldURL := "/assets/app.a1.js"
	oldAsset := request(h, "GET", "foo.example.com", oldURL, map[string]string{"Accept": "text/html"})
	if oldAsset.Code != 200 || oldAsset.Body.String() != "old javascript" || oldAsset.Header().Get("ETag") != `"`+old.Files["assets/app.a1.js"].SHA256+`"` {
		t.Fatalf("previous asset or SPA precedence: %d %q %v", oldAsset.Code, oldAsset.Body.String(), oldAsset.Header())
	}
	rangeResponse := request(h, "GET", "foo.example.com", oldURL, map[string]string{"Range": "bytes=0-2"})
	if rangeResponse.Code != 206 || rangeResponse.Body.String() != "old" {
		t.Fatalf("retained asset Range: %d %q", rangeResponse.Code, rangeResponse.Body.String())
	}
	head := request(h, "HEAD", "foo.example.com", oldURL, nil)
	if head.Code != 200 || head.Body.Len() != 0 || head.Header().Get("Content-Length") != "14" {
		t.Fatalf("retained asset HEAD: %d %q %v", head.Code, head.Body.String(), head.Header())
	}
	conditional := request(h, "GET", "foo.example.com", oldURL, map[string]string{"If-None-Match": oldAsset.Header().Get("ETag")})
	if conditional.Code != 304 || conditional.Body.Len() != 0 {
		t.Fatalf("retained asset validator: %d %q", conditional.Code, conditional.Body.String())
	}
	newAsset := request(h, "GET", "foo.example.com", "/assets/app.b2.js", nil)
	if newAsset.Code != 200 || newAsset.Body.String() != "new javascript" {
		t.Fatalf("current asset: %d %q", newAsset.Code, newAsset.Body.String())
	}
	for _, tc := range []struct{ host, target string }{
		{"foo.example.com", "/old.html"}, {"foo.example.com", "/assets/secret.js"},
		{deploy.Host("example.com", "flat", "foo", token), oldURL},
	} {
		w := request(h, "GET", tc.host, tc.target, map[string]string{"Accept": "application/javascript"})
		if w.Code != 404 {
			t.Fatalf("retention scope %s%s: %d %q", tc.host, tc.target, w.Code, w.Body.String())
		}
	}
	// Expiry is checked for every request, even when the cached routing snapshot
	// has not yet been refreshed and the old objects still physically exist.
	now = now.Add(2 * time.Hour)
	w := request(h, "GET", "foo.example.com", oldURL, nil)
	if w.Code != 404 {
		t.Fatalf("expired cached lease served: %d", w.Code)
	}
	if w = request(h, "GET", "foo.example.com", "/", nil); w.Code != 200 || w.Body.String() != "new HTML" {
		t.Fatalf("production expired with assets: %d %q", w.Code, w.Body.String())
	}
}

func TestNavigationDetection(t *testing.T) {
	for _, tc := range []struct {
		path, accept, destination string
		want                      bool
	}{
		{"deep/path", "", "", true}, {"deep/path", "application/json", "", false},
		{"script.js", "*/*", "script", false}, {"script.js", "text/html", "script", false},
		{"page", "text/html;q=0", "", false}, {"page", "text/html;q=0.8", "document", true},
		{"alice.smith", "text/html", "document", true},
	} {
		r := httpRequestForNavigation(tc.accept, tc.destination)
		if got := isNavigation(r, tc.path); got != tc.want {
			t.Fatalf("%+v: got %v", tc, got)
		}
	}
}

func TestRoutingFilesRollbackAndDisableTogether(t *testing.T) {
	ctx := context.Background()
	c := deploy.New(storage.NewMemory())
	first := publish(t, c, "foo", "routes-first", map[string]string{
		"pages.json": `{"schemaVersion":1,"routing":{"routes":[{"from":"/entry","rewrite":"/one.html"}]}}`,
		"one.html":   "first page",
	})
	save(t, c, first, nil)
	h := handler(t, c, "flat")
	second := publish(t, c, "foo", "routes-second", map[string]string{
		"pages.json": `{"schemaVersion":1,"routing":{"routes":[{"from":"/entry","rewrite":"/two.html"}]}}`,
		"two.html":   "second page",
	})
	save(t, c, second, nil)
	if w := request(h, "GET", "foo.example.com", "/entry", nil); w.Code != 200 || w.Body.String() != "first page" {
		t.Fatalf("snapshot mixed before refresh: %d %q", w.Code, w.Body.String())
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "GET", "foo.example.com", "/entry", nil); w.Code != 200 || w.Body.String() != "second page" {
		t.Fatalf("new route/file mismatch: %d %q", w.Code, w.Body.String())
	}
	if _, err := c.Rollback(ctx, "foo", first.Owner, ""); err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "GET", "foo.example.com", "/entry", nil); w.Code != 200 || w.Body.String() != "first page" {
		t.Fatalf("rollback route/file mismatch: %d %q", w.Code, w.Body.String())
	}
	if _, err := c.SetControl(ctx, "foo", first.Owner, "disabled", ""); err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	if w := request(h, "GET", "foo.example.com", "/entry", nil); w.Code != 404 {
		t.Fatalf("disabled site still serves: %d", w.Code)
	}
}

func httpRequestForNavigation(accept, destination string) *http.Request {
	r, _ := http.NewRequest("GET", "http://example.com/", strings.NewReader(""))
	r.Header.Set("Accept", accept)
	r.Header.Set("Sec-Fetch-Dest", destination)
	return r
}
