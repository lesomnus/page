package githubsync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
)

func sha(n int) string { return fmt.Sprintf("%040x", n) }

type fakeGitHub struct {
	mu            sync.Mutex
	head          string
	branches      map[string]string
	tags          map[string]string
	parents       map[string][]string
	manifests     map[string]string
	archives      map[string][]byte
	archiveReads  int
	pageFailure   bool
	repeatedPage  bool
	branchFailure bool
}

func fixture(t *testing.T) (*fakeGitHub, *httptest.Server) {
	t.Helper()
	f := &fakeGitHub{head: sha(2), branches: map[string]string{"main": sha(2), "feature/login": sha(4)}, tags: map[string]string{"v1.0": sha(1)}, parents: map[string][]string{sha(2): {sha(1), sha(9)}, sha(1): {}, sha(3): {sha(2)}, sha(4): {}, sha(9): {}}, manifests: map[string]string{}, archives: map[string][]byte{}}
	for _, id := range []string{sha(1), sha(2), sha(3), sha(4), sha(9)} {
		f.manifests[id] = `{"schemaVersion":1,"publish":{"directory":"public"}}`
		f.archives[id] = tarball(t, []tarEntry{{"public/index.html", "snapshot-" + id, tar.TypeReg}, {"public/assets/app.js", "console.log('ok')", tar.TypeReg}})
	}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, server
}

func (f *fakeGitHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	write := func(v any) { json.NewEncoder(w).Encode(v) }
	switch {
	case r.URL.Path == "/orgs/acme/repos":
		if f.repeatedPage {
			w.Header().Set("Link", `<http://unused?page=2>; rel="next"`)
		}
		if f.pageFailure {
			if r.URL.Query().Get("page") == "2" {
				http.Error(w, "failed", 500)
				return
			}
			w.Header().Set("Link", `<http://unused?page=2>; rel="next"`)
		}
		write([]repository{{ID: 42, Name: "foo.app", FullName: "acme/foo.app", DefaultBranch: "main"}})
	case r.URL.Path == "/repos/acme/foo.app/branches":
		if f.branchFailure {
			http.Error(w, "failed", 500)
			return
		}
		var out []namedCommit
		for n, s := range f.branches {
			out = append(out, namedCommit{Name: n, Commit: commitID{s}})
		}
		write(out)
	case r.URL.Path == "/repos/acme/foo.app/tags":
		var out []namedCommit
		for n, s := range f.tags {
			out = append(out, namedCommit{Name: n, Commit: commitID{s}})
		}
		write(out)
	case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.app/commits/"):
		id := strings.TrimPrefix(r.URL.Path, "/repos/acme/foo.app/commits/")
		if id == "main" && f.tags["main"] != "" {
			id = f.tags["main"]
		} else if id == "main" || id == "refs/heads/main" {
			id = f.head
		} else if strings.HasPrefix(id, "refs/heads/") {
			id = f.branches[strings.TrimPrefix(id, "refs/heads/")]
		}
		parents, exists := f.parents[id]
		if !exists {
			http.NotFound(w, r)
			return
		}
		c := commit{SHA: id}
		for _, p := range parents {
			c.Parents = append(c.Parents, commitID{p})
		}
		write(c)
	case r.URL.Path == "/repos/acme/foo.app/contents/manifest.json":
		manifest, ok := f.manifests[r.URL.Query().Get("ref")]
		if !ok {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, manifest)
	case strings.HasPrefix(r.URL.Path, "/repos/acme/foo.app/tarball/"):
		id := strings.TrimPrefix(r.URL.Path, "/repos/acme/foo.app/tarball/")
		data, ok := f.archives[id]
		if !ok {
			http.NotFound(w, r)
			return
		}
		f.archiveReads++
		w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func TestProductionExplicitlyResolvesBranchWhenTagHasSameName(t *testing.T) {
	f, api := fixture(t)
	f.mu.Lock()
	f.tags["main"] = sha(1)
	f.mu.Unlock()
	for _, branch := range []string{"main", "refs/heads/main"} {
		t.Run(branch, func(t *testing.T) {
			cfg := config(api.URL)
			cfg.Branch = branch
			cfg.Tags = []string{"*"}
			cat := deploy.New(storage.NewMemory())
			mustOnce(t, newSync(t, cat, cfg))
			site := getSite(t, cat)
			production, err := cat.GetRelease(context.Background(), site.Owner, site.Name, site.Production)
			if err != nil {
				t.Fatal(err)
			}
			if production.Origin.Revision != sha(2) {
				t.Fatalf("production selected tag instead of branch: %s", production.Origin.Revision)
			}
			if site.Aliases[deploy.AliasToken("tag", "refs/tags/main")].Revision != sha(1) {
				t.Fatal("same-name tag preview did not retain its own snapshot")
			}
		})
	}
}

func TestIncompleteFirstPublicationIsCollectedOnlyAfterSuccessfulScan(t *testing.T) {
	ctx := context.Background()
	f, api := fixture(t)
	store := storage.NewMemory()
	cat := deploy.New(store)
	cfg := config(api.URL)
	origin := deploy.Origin{Kind: "git", RepositoryID: "42", Revision: sha(2), Directory: "public"}
	_, err := cat.Publish(ctx, "foo", cfg.ID, origin, []deploy.InputFile{{Path: "index.html", Size: 1, Open: func() (io.ReadCloser, error) { return nil, errors.New("first upload interrupted") }}})
	if err == nil {
		t.Fatal("expected initial publication failure")
	}
	id := deploy.ReleaseID(cfg.ID, "foo", origin)
	key := deploy.ReleasePrefix(cfg.ID, "foo", id) + "upload.json"
	f.mu.Lock()
	f.manifests = map[string]string{}
	f.branchFailure = true
	f.mu.Unlock()
	s := newSync(t, cat, cfg)
	s.now = func() time.Time { return time.Now().UTC().Add(time.Hour) }
	if err := s.Once(ctx); err == nil {
		t.Fatal("source failure unexpectedly succeeded")
	}
	if _, err := store.Stat(ctx, key); err != nil {
		t.Fatal("source failure removed pending publication:", err)
	}
	f.mu.Lock()
	f.branchFailure = false
	f.mu.Unlock()
	mustOnce(t, s)
	if _, err := store.Stat(ctx, key); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("orphan first upload was not collected:", err)
	}
	if _, err := cat.GetSite(ctx, "foo"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("cleanup published an empty site:", err)
	}
}

func TestForcePushAndRepeatedPagination(t *testing.T) {
	ctx := context.Background()
	f, api := fixture(t)
	cat := deploy.New(storage.NewMemory())
	s := newSync(t, cat, config(api.URL))
	mustOnce(t, s)
	f.mu.Lock()
	f.head = sha(1)
	f.branches["main"] = sha(1)
	f.mu.Unlock()
	mustOnce(t, s)
	site := getSite(t, cat)
	r, err := cat.GetRelease(ctx, site.Owner, site.Name, site.Production)
	if err != nil {
		t.Fatal(err)
	}
	if r.Origin.Revision != sha(1) {
		t.Fatal("force-push to an older commit did not move production")
	}
	before, _ := json.Marshal(site)
	f.mu.Lock()
	f.repeatedPage = true
	f.mu.Unlock()
	if err := s.Once(ctx); err == nil || !strings.Contains(err.Error(), "repeated a page") {
		t.Fatal("repeated pagination was not detected:", err)
	}
	after, _ := json.Marshal(getSite(t, cat))
	if !bytes.Equal(before, after) {
		t.Fatal("broken pagination changed published metadata")
	}
}

func TestBranchPatternWildcardAndArchiveHeaderLimit(t *testing.T) {
	if !matches([]string{"*"}, "feature/team/login") || !matches([]string{"feature/*"}, "feature/login") || matches([]string{"feature/*"}, "feature/team/login") {
		t.Fatal("unexpected branch pattern semantics")
	}
	f, api := fixture(t)
	f.mu.Lock()
	f.archives[sha(2)] = tarball(t, []tarEntry{{"outside/a", "", tar.TypeReg}, {"outside/b", "", tar.TypeReg}, {"outside/c", "", tar.TypeReg}, {"public/index.html", "ok", tar.TypeReg}})
	f.mu.Unlock()
	cfg := config(api.URL)
	cfg.MaxArchiveBytes = 1024
	s := newSync(t, deploy.New(storage.NewMemory()), cfg)
	if _, cleanup, err := s.archive(context.Background(), "acme/foo.app", sha(2), "public"); err == nil {
		cleanup()
		t.Fatal("archive header expansion escaped archive size limit")
	}
}

func TestPersonalRepositoryDiscoveryUsesAuthenticatedEndpointAndFiltersOwner(t *testing.T) {
	for _, authenticated := range []bool{false, true} {
		t.Run(fmt.Sprintf("authenticated-%t", authenticated), func(t *testing.T) {
			f, _ := fixture(t)
			var listingCalls int
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expected := "/users/ACME/repos"
				if authenticated {
					expected = "/user/repos"
				}
				if r.URL.Path == expected {
					listingCalls++
					if authenticated && (r.Header.Get("Authorization") != "Bearer user-token" || r.URL.Query().Get("affiliation") != "owner") {
						t.Error("authenticated personal repository listing lacks token or owner affiliation")
					}
					if !authenticated && r.Header.Get("Authorization") != "" {
						t.Error("public repository discovery sent credentials")
					}
					w.Header().Set("Content-Type", "application/json")
					// A token's owned list can contain another owner in an imperfect
					// provider response; that entry must not claim the same site.
					json.NewEncoder(w).Encode([]map[string]any{{"id": 43, "name": "foo.app", "full_name": "another/foo.app", "default_branch": "main", "private": true}, {"id": 42, "name": "foo.app", "full_name": "acme/foo.app", "default_branch": "main", "private": authenticated}})
					return
				}
				f.ServeHTTP(w, r)
			}))
			defer api.Close()
			cfg := config(api.URL)
			cfg.Owner = "ACME"
			cfg.OwnerType = "user"
			if authenticated {
				cfg.Token = "user-token"
			}
			cat := deploy.New(storage.NewMemory())
			mustOnce(t, newSync(t, cat, cfg))
			site := getSite(t, cat)
			if site.RepositoryID != "42" || site.Repository != "acme/foo.app" || listingCalls != 1 {
				t.Fatalf("wrong personal repository discovered: %+v, calls %d", site, listingCalls)
			}
		})
	}
}

type tarEntry struct {
	name, body string
	kind       byte
}

func tarball(t *testing.T, entries []tarEntry) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{Name: "acme-foo-abc/", Typeflag: tar.TypeDir, Mode: 0755}); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		size := int64(len(e.body))
		h := &tar.Header{Name: "acme-foo-abc/" + e.name, Mode: 0644, Typeflag: e.kind, Size: size}
		if e.kind == tar.TypeSymlink {
			h.Linkname = e.body
			h.Size = 0
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if e.kind == tar.TypeReg {
			if _, err := io.WriteString(tw, e.body); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func config(url string) Config {
	return Config{ID: "github-acme", Owner: "acme", APIURL: url, RecentCommits: 2, GCGrace: time.Second, Branches: []string{"*"}, Tags: []string{"v*"}}
}
func newSync(t *testing.T, cat *deploy.Catalog, cfg Config) *Sync {
	t.Helper()
	s, err := New(cat, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func mustOnce(t *testing.T, s *Sync) {
	t.Helper()
	if err := s.Once(context.Background()); err != nil {
		t.Fatal(err)
	}
}
func getSite(t *testing.T, cat *deploy.Catalog) *deploy.Site {
	t.Helper()
	s, err := cat.GetSite(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func assertHTTP(t *testing.T, h http.Handler, host string, status int, body string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != status || (body != "" && w.Body.String() != body) {
		t.Fatalf("%s: got %d %q, want %d %q", host, w.Code, w.Body.String(), status, body)
	}
}

func TestGitHubToServingAndSourceOwnedGC(t *testing.T) {
	ctx := context.Background()
	f, api := fixture(t)
	store := storage.NewMemory()
	cat := deploy.New(store)
	cfg := config(api.URL)
	s := newSync(t, cat, cfg)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	mustOnce(t, s)
	site := getSite(t, cat)
	prod, err := cat.GetRelease(ctx, site.Owner, site.Name, site.Production)
	if err != nil {
		t.Fatal(err)
	}
	if prod.Origin.Revision != sha(2) {
		t.Fatal("wrong production snapshot")
	}
	if _, exists := site.Aliases[deploy.AliasToken("commit", sha(9))]; exists {
		t.Fatal("first-parent selection included a merged side parent")
	}
	h, err := serving.New(cat, serving.Config{BaseDomain: "example.com"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	assertHTTP(t, h, "foo.example.com", 200, "snapshot-"+sha(2))
	for token, alias := range site.Aliases {
		assertHTTP(t, h, deploy.Host("example.com", "flat", "foo", token), 200, "snapshot-"+alias.Revision)
	}
	assertHTTP(t, h, "other.example.com", 404, "")
	oldID := site.Aliases[deploy.AliasToken("commit", sha(1))].Release
	old, err := cat.GetRelease(ctx, site.Owner, site.Name, oldID)
	if err != nil {
		t.Fatal(err)
	}
	// A different owner's publication must remain untouched by this sync's GC.
	other, err := cat.Publish(ctx, "bar", "other-source", deploy.Origin{Kind: "git", RepositoryID: "99", Revision: sha(99)}, []deploy.InputFile{{Path: "index.html", Size: 3, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("bar")), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveSite(ctx, &deploy.Site{SchemaVersion: 1, Name: "bar", Owner: "other-source", RepositoryID: "99", Production: other.ID, Aliases: map[string]deploy.Alias{}}); err != nil {
		t.Fatal(err)
	}
	// An interrupted upload is recoverable and can be collected by its owner.
	unfinishedOrigin := deploy.Origin{Kind: "git", RepositoryID: "42", Revision: sha(55)}
	_, err = cat.Publish(ctx, "foo", cfg.ID, unfinishedOrigin, []deploy.InputFile{{Path: "index.html", Open: func() (io.ReadCloser, error) { return nil, errors.New("interrupted") }}})
	if err == nil {
		t.Fatal("expected failed upload")
	}
	pendingID := deploy.ReleaseID(cfg.ID, "foo", unfinishedOrigin)
	f.mu.Lock()
	f.head = sha(3)
	f.branches = map[string]string{"main": sha(3)}
	f.tags = map[string]string{}
	f.mu.Unlock()
	cfg.RecentCommits = 1
	cfg.Tags = []string{}
	s = newSync(t, cat, cfg)
	s.now = func() time.Time { return now }
	now = now.Add(time.Hour)
	mustOnce(t, s)
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	assertHTTP(t, h, "foo.example.com", 200, "snapshot-"+sha(3))
	assertHTTP(t, h, deploy.Host("example.com", "flat", "foo", deploy.AliasToken("commit", sha(1))), 404, "")
	if _, err := store.Stat(ctx, deploy.FileKey(old, "index.html")); err != nil {
		t.Fatal("old release removed before grace:", err)
	}
	now = now.Add(2 * time.Second)
	// Restart restores retirements from Storage rather than process memory.
	s = newSync(t, cat, cfg)
	s.now = func() time.Time { return now }
	mustOnce(t, s)
	if _, err := store.Stat(ctx, deploy.FileKey(old, "index.html")); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("expired release still exists:", err)
	}
	if _, err := store.Stat(ctx, deploy.ReleasePrefix(cfg.ID, "foo", pendingID)+"upload.json"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("unfinished upload still exists:", err)
	}
	if _, err := store.Stat(ctx, deploy.FileKey(other, "index.html")); err != nil {
		t.Fatal("other owner's release removed:", err)
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	assertHTTP(t, h, "foo.example.com", 200, "snapshot-"+sha(3))
	f.mu.Lock()
	reads := f.archiveReads
	f.mu.Unlock()
	mustOnce(t, newSync(t, cat, cfg))
	f.mu.Lock()
	defer f.mu.Unlock()
	if reads != f.archiveReads {
		t.Fatal("restart uploaded an existing revision again")
	}
}

func TestFailedScanOrPublicationPreservesAliases(t *testing.T) {
	for _, failure := range []string{"repository-pagination", "branch-list", "invalid-manifest", "archive-download"} {
		t.Run(failure, func(t *testing.T) {
			f, api := fixture(t)
			cat := deploy.New(storage.NewMemory())
			s := newSync(t, cat, config(api.URL))
			mustOnce(t, s)
			before := getSite(t, cat)
			oldJSON, _ := json.Marshal(before)
			f.mu.Lock()
			f.head = sha(3)
			f.branches = map[string]string{"main": sha(3)}
			f.tags = map[string]string{}
			switch failure {
			case "repository-pagination":
				f.pageFailure = true
			case "branch-list":
				f.branchFailure = true
			case "invalid-manifest":
				f.manifests[sha(3)] = `{"schemaVersion":1,"publish":{"directory":"../private"}}`
			case "archive-download":
				delete(f.archives, sha(3))
			}
			f.mu.Unlock()
			if err := s.Once(context.Background()); err == nil {
				t.Fatal("failed scan unexpectedly succeeded")
			}
			after := getSite(t, cat)
			newJSON, _ := json.Marshal(after)
			if !bytes.Equal(oldJSON, newJSON) {
				t.Fatalf("failed pass changed publication:\n%s\n%s", oldJSON, newJSON)
			}
		})
	}
}

func TestMissingManifestAndBranchTagNames(t *testing.T) {
	f, api := fixture(t)
	cat := deploy.New(storage.NewMemory())
	cfg := config(api.URL)
	f.mu.Lock()
	f.branches["release"] = sha(2)
	f.tags["release"] = sha(1)
	f.mu.Unlock()
	cfg.Tags = []string{"*"}
	s := newSync(t, cat, cfg)
	mustOnce(t, s)
	site := getSite(t, cat)
	b := site.Aliases[deploy.AliasToken("branch", "refs/heads/release")]
	tag := site.Aliases[deploy.AliasToken("tag", "refs/tags/release")]
	if b.Release == tag.Release || b.Revision != sha(2) || tag.Revision != sha(1) {
		t.Fatal("branch/tag namespace collision")
	}
	f.mu.Lock()
	f.head = sha(3)
	f.branches["release"] = sha(3)
	delete(f.manifests, sha(3))
	f.mu.Unlock()
	mustOnce(t, s)
	after := getSite(t, cat)
	if after.Production != site.Production || after.Aliases[deploy.AliasToken("branch", "refs/heads/release")].Release != b.Release {
		t.Fatal("missing manifest removed a previous successful snapshot")
	}
	// A newly discovered repository with no deployable production stays absent.
	empty := deploy.New(storage.NewMemory())
	mustOnce(t, newSync(t, empty, cfg))
	if _, err := empty.GetSite(context.Background(), "foo"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatal("missing production manifest published a site")
	}
}

func TestArchiveRejectsEscapesLinksAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []tarEntry
		limit   int64
	}{
		{"traversal", []tarEntry{{"public/../escape", "bad", tar.TypeReg}}, 1000},
		{"symlink", []tarEntry{{"public/link", "../../etc/passwd", tar.TypeSymlink}}, 1000},
		{"oversize", []tarEntry{{"public/index.html", strings.Repeat("x", 100), tar.TypeReg}}, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, api := fixture(t)
			f.mu.Lock()
			f.archives[sha(2)] = tarball(t, tc.entries)
			f.mu.Unlock()
			cfg := config(api.URL)
			cfg.MaxFileBytes = tc.limit
			s := newSync(t, deploy.New(storage.NewMemory()), cfg)
			if _, cleanup, err := s.archive(context.Background(), "acme/foo.app", sha(2), "public"); err == nil {
				cleanup()
				t.Fatal("unsafe archive accepted")
			}
		})
	}
}
