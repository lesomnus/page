package app

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/githubsync"
	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
	"gopkg.in/yaml.v3"
)

// Exercise release routing and operator intent across independent CLI/catalog
// instances, the forge protocol, live S3 metadata and HTTP publication.
func TestExpansionWithS3(t *testing.T) {
	endpoint := os.Getenv("PAGE_TEST_S3_ENDPOINT")
	if endpoint == "" {
		t.Skip("set PAGE_TEST_S3_ENDPOINT to exercise a live S3 API")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Second)
	defer cancel()
	sha1, sha2, sha3 := strings.Repeat("a", 40), strings.Repeat("b", 40), strings.Repeat("c", 40)
	pages := `{"schemaVersion":1,"routing":{"cleanUrls":true,"routes":[{"from":"/guide/:name","rewrite":"/docs/:name.html"},{"from":"/legacy","redirect":"/about","status":308}],"fallback":{"rewrite":"/index.html","exclude":["/api/*","/assets/*"]},"errorPages":{"404":"/404.html"}},"headers":[{"from":"/guide/*","set":{"X-Route":"guide"}},{"from":"/assets/*","set":{"Cache-Control":"public, max-age=31536000, immutable"}}],"assets":{"immutable":["/assets/*"]}}`
	files := func(version string) map[string]string {
		return map[string]string{
			"pages.json": pages, "index.html": "app " + version,
			"about.html": "about " + version, "docs/start.html": "guide " + version,
			"404.html": "missing " + version, "assets/app." + version + ".js": "bundle " + version,
		}
	}
	one, two := files("one"), files("two")
	one["only-one.html"] = "old page"
	archive := func(files map[string]string) []byte {
		t.Helper()
		var b bytes.Buffer
		gz := gzip.NewWriter(&b)
		tr := tar.NewWriter(gz)
		paths := make([]string, 0, len(files))
		for path := range files {
			paths = append(paths, path)
		}
		sort.Strings(paths)
		for _, path := range paths {
			body := files[path]
			if err := tr.WriteHeader(&tar.Header{Name: "repo/public/" + path, Size: int64(len(body)), Mode: 0644}); err != nil {
				t.Fatal(err)
			}
			if _, err := io.WriteString(tr, body); err != nil {
				t.Fatal(err)
			}
		}
		if err := tr.Close(); err != nil {
			t.Fatal(err)
		}
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
		return b.Bytes()
	}
	invalid := files("three")
	invalid["pages.json"] = `{"schemaVersion":99}`
	archives := map[string][]byte{sha1: archive(one), sha2: archive(two), sha3: archive(invalid)}
	var head atomic.Value
	head.Store(sha1)
	var offline atomic.Bool
	forge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if offline.Load() {
			http.Error(w, "forge offline", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/orgs/acme/repos":
			io.WriteString(w, `[{"id":42,"name":"foo.app","full_name":"acme/foo.app","default_branch":"main"}]`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.app/commits/"):
			json.NewEncoder(w).Encode(map[string]any{"sha": head.Load().(string), "parents": []any{}})
		case r.URL.Path == "/repos/acme/foo.app/branches":
			json.NewEncoder(w).Encode([]any{map[string]any{"name": "main", "commit": map[string]string{"sha": head.Load().(string)}}})
		case r.URL.Path == "/repos/acme/foo.app/contents/manifest.json":
			io.WriteString(w, `{"schemaVersion":1,"publish":{"directory":"public"}}`)
		case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.app/tarball/"):
			if body, ok := archives[strings.TrimPrefix(r.URL.Path, "/repos/acme/foo.app/tarball/")]; ok {
				w.Write(body)
			} else {
				http.NotFound(w, r)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	defer forge.Close()
	bucket := os.Getenv("PAGE_TEST_S3_BUCKET")
	if bucket == "" {
		bucket = "page-storage-test"
	}
	cfg := Config{BaseDomain: "example.com", PreviewStyle: "flat", Storage: storage.S3Config{Bucket: bucket, Prefix: "expansion-" + strconv.FormatInt(time.Now().UnixNano(), 36), Region: "us-east-1", Endpoint: endpoint, PathStyle: true}, GitHub: []githubsync.Config{{ID: "source", Owner: "acme", APIURL: forge.URL, Branches: []string{"main"}, RecentCommits: 1, GCGrace: time.Millisecond, AssetRetention: time.Hour}}}
	configJSON, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	configPath := configFile(t, string(configJSON))
	store, err := storage.NewS3(ctx, cfg.Storage)
	if err != nil {
		t.Fatal(err)
	}
	cat := deploy.New(store)
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cursor := ""
		var keys []string
		for {
			page, err := store.List(cleanup, "", cursor, 1000)
			if err != nil {
				t.Log(err)
				return
			}
			for _, object := range page.Objects {
				keys = append(keys, object.Key)
			}
			if page.NextCursor == "" {
				break
			}
			cursor = page.NextCursor
		}
		for _, key := range keys {
			if err := store.Delete(cleanup, key); err != nil {
				t.Log(err)
			}
		}
	})
	cli := func(args ...string) string {
		t.Helper()
		var out, errOut bytes.Buffer
		if err := Run(ctx, args, &out, &errOut); err != nil {
			t.Fatalf("page %v: %v (%s)", args, err, errOut.String())
		}
		return out.String()
	}
	syncOnce := func() { t.Helper(); cli("sync", "--once", "--config", configPath) }
	site := func() *deploy.Site {
		t.Helper()
		s, err := cat.GetSite(ctx, "foo")
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	intent := func(command string, target ...string) {
		t.Helper()
		before, _ := json.Marshal(site())
		args := append([]string{command, "foo"}, target...)
		args = append(args, "--config", configPath, "--source", "source")
		var status controlStatus
		if err := json.Unmarshal([]byte(cli(args...)), &status); err != nil {
			t.Fatal(err)
		}
		if status.Applied {
			t.Fatal("external CLI claimed intent was applied without an owner pass")
		}
		after, _ := json.Marshal(site())
		if !bytes.Equal(before, after) {
			t.Fatal("external CLI changed publication instead of only writing intent")
		}
	}
	local := t.TempDir()
	for path, body := range one {
		name := filepath.Join(local, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if !strings.Contains(cli("routes", "validate", local), `"valid": true`) {
		t.Fatal("local release validation did not succeed")
	}
	var decision routes.Decision
	if err := json.Unmarshal([]byte(cli("routes", "test", local, "/guide/start")), &decision); err != nil || decision.Path != "docs/start.html" || decision.Headers["X-Route"] != "guide" {
		t.Fatalf("local route decision: %+v %v", decision, err)
	}
	syncOnce()
	oldID := site().Production
	h, err := serving.New(deploy.New(store), serving.Config{BaseDomain: cfg.BaseDomain})
	if err != nil {
		t.Fatal(err)
	}
	request := func(host, path, accept string, status int, body string) *httptest.ResponseRecorder {
		t.Helper()
		if err := h.Refresh(ctx); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodGet, "http://"+host+path, nil)
		if accept != "" {
			r.Header.Set("Accept", accept)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != status || (body != "" && w.Body.String() != body) {
			t.Fatalf("%s%s: status %d body %q; want %d %q", host, path, w.Code, w.Body.String(), status, body)
		}
		return w
	}
	const production = "foo.example.com"
	request(production, "/about", "", 200, "about one")
	if w := request(production, "/guide/start", "", 200, "guide one"); w.Header().Get("X-Route") != "guide" {
		t.Fatal("rewritten response lost original-path headers")
	}
	request(production, "/dashboard", "text/html", 200, "app one")
	request(production, "/api/missing", "text/html", 404, "missing one")
	if w := request(production, "/legacy?query=kept", "", 308, ""); w.Header().Get("Location") != "/about?query=kept" {
		t.Fatal("internal redirect lost its target or query")
	}
	intent("pause")
	head.Store(sha2)
	syncOnce()
	request(production, "/", "", 200, "app one")
	branchHost := deploy.Host(cfg.BaseDomain, "flat", "foo", deploy.AliasToken("branch", "refs/heads/main"))
	request(branchHost, "/", "", 200, "app two")
	newID := site().Aliases[deploy.AliasToken("commit", sha2)].Release
	intent("resume")
	syncOnce()
	request(production, "/", "", 200, "app two")
	asset := request(production, "/assets/app.one.js", "application/javascript", 200, "bundle one")
	if !strings.Contains(asset.Header().Get("Cache-Control"), "immutable") {
		t.Fatal("retained asset lost its configured cache policy")
	}
	request(production, "/only-one.html", "application/octet-stream", 404, "missing two")
	request(branchHost, "/assets/app.one.js", "application/javascript", 404, "missing two")
	oldHost := deploy.Host(cfg.BaseDomain, "flat", "foo", deploy.AliasToken("commit", sha1))
	request(oldHost, "/", "", 404, "")
	if site().AssetReleases[oldID].IsZero() {
		t.Fatal("previous production asset retention was not durable")
	}
	intent("rollback")
	syncOnce()
	request(production, "/", "", 200, "app one")
	syncOnce()
	request(production, "/", "", 200, "app one")
	intent("pin")
	syncOnce()
	if site().Production != oldID {
		t.Fatal("pin did not survive another source scan")
	}
	intent("promote", newID)
	syncOnce()
	request(production, "/", "", 200, "app two")
	head.Store(sha3)
	before, _ := json.Marshal(site())
	var out, errOut bytes.Buffer
	if err := Run(ctx, []string{"sync", "--once", "--config", configPath}, &out, &errOut); err == nil {
		t.Fatal("invalid pages.json became publishable")
	}
	after, _ := json.Marshal(site())
	if !bytes.Equal(before, after) {
		t.Fatal("invalid new release changed the published site")
	}
	request(production, "/", "", 200, "app two")
	intent("remove")
	syncOnce()
	request(production, "/", "", 404, "")
	request(branchHost, "/", "", 404, "")
	withdrawn := site()
	if !withdrawn.Disabled || len(withdrawn.AssetReleases) != 0 {
		t.Fatal("removal retained public asset leases")
	}
	// Backdate only source-owned retirement timestamps to test GC without a
	// scheduler-dependent sleep. The durable disabled control stays authoritative.
	for id := range withdrawn.Retired {
		withdrawn.Retired[id] = time.Now().Add(-time.Hour)
	}
	if err := cat.SaveSite(ctx, withdrawn); err != nil {
		t.Fatal(err)
	}
	offline.Store(true)
	if err := Run(ctx, []string{"sync", "--once", "--config", configPath}, &out, &errOut); err == nil {
		t.Fatal("forge outage was not reported")
	}
	for _, id := range []string{oldID, newID} {
		if _, err := cat.GetRelease(ctx, "source", "foo", id); !errors.Is(err, storage.ErrNotFound) {
			t.Fatalf("withdrawn owned release %s was not collected during outage: %v", id, err)
		}
	}
	request(production, "/assets/app.one.js", "", 404, "")
}
