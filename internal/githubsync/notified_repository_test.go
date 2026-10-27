package githubsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

type targetedForge struct {
	github    *fakeGitHub
	mu        sync.Mutex
	repos     map[string]repository
	overrides map[string]repository
	statuses  map[string]int
	failures  map[string]int
	calls     []string
	gatePath  string
	entered   chan struct{}
	release   chan struct{}
}

func targetedFixture(t *testing.T) (*targetedForge, *httptest.Server) {
	t.Helper()
	github, _ := fixture(t)
	f := &targetedForge{github: github, repos: map[string]repository{
		"acme/foo.app": {ID: 42, Name: "foo.app", FullName: "acme/foo.app", DefaultBranch: "main"},
		"acme/bar.app": {ID: 43, Name: "bar.app", FullName: "acme/bar.app", DefaultBranch: "main"},
	}, overrides: map[string]repository{}, statuses: map[string]int{}, failures: map[string]int{}}
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	return f, server
}

func (f *targetedForge) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.Path)
	var gate, release chan struct{}
	if r.URL.Path == f.gatePath {
		gate, release, f.gatePath = f.entered, f.release, ""
	}
	status := f.statuses[r.URL.Path]
	if f.failures[r.URL.Path] > 0 {
		f.failures[r.URL.Path]--
		status = http.StatusServiceUnavailable
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/repos/"), "/")
	fullName := ""
	if len(parts) >= 2 {
		fullName = parts[0] + "/" + parts[1]
	}
	repo, exists := f.repos[fullName]
	if override, ok := f.overrides[fullName]; ok {
		repo = override
	}
	repos := make([]repository, 0, len(f.repos))
	for _, repository := range f.repos {
		repos = append(repos, repository)
	}
	f.mu.Unlock()
	if gate != nil {
		close(gate)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
	}
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if r.URL.Path == "/orgs/acme/repos" {
		sort.Slice(repos, func(i, j int) bool { return repos[i].FullName < repos[j].FullName })
		json.NewEncoder(w).Encode(repos)
		return
	}
	if !exists {
		http.NotFound(w, r)
		return
	}
	if len(parts) == 2 {
		json.NewEncoder(w).Encode(repo)
		return
	}
	// Reuse the fixture's branch/commit/archive protocol for each independently
	// identified repository. All paths are recorded before this local rewrite.
	copy := r.Clone(r.Context())
	u := *r.URL
	u.Path = strings.Replace(r.URL.Path, "/repos/"+fullName+"/", "/repos/acme/foo.app/", 1)
	copy.URL = &u
	f.github.ServeHTTP(w, copy)
}

func (f *targetedForge) paths() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *targetedForge) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

func (f *targetedForge) advance() {
	f.github.mu.Lock()
	f.github.head = sha(3)
	f.github.branches["main"] = sha(3)
	f.github.mu.Unlock()
}

type targetedStore struct {
	storage.Store
	mu      sync.Mutex
	writes  []string
	deletes []string
}

func (s *targetedStore) Put(ctx context.Context, key string, body io.Reader, size int64, contentType string) (storage.Info, error) {
	s.mu.Lock()
	s.writes = append(s.writes, key)
	s.mu.Unlock()
	return s.Store.Put(ctx, key, body, size, contentType)
}

func (s *targetedStore) Delete(ctx context.Context, key string) error {
	s.mu.Lock()
	s.deletes = append(s.deletes, key)
	s.mu.Unlock()
	return s.Store.Delete(ctx, key)
}

func (s *targetedStore) reset() {
	s.mu.Lock()
	s.writes, s.deletes = nil, nil
	s.mu.Unlock()
}

func targetedSource(t *testing.T, server string, store storage.Store) (*Sync, *deploy.Catalog) {
	t.Helper()
	cat := deploy.New(store)
	s, err := New(cat, Config{ID: "source", Owner: "acme", APIURL: server, PollInterval: time.Hour, Branches: []string{"main"}, RecentCommits: 1, GCGrace: time.Nanosecond})
	if err != nil {
		t.Fatal(err)
	}
	return s, cat
}

func waitTarget(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("targeted source did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func startTargeted(t *testing.T, s *Sync, f *targetedForge) {
	t.Helper()
	runNotificationSource(t, s)
	waitTarget(t, func() bool { return len(f.paths()) > 0 })
	// The first HTTP call happens while Once holds the writer lock. Waiting for
	// that lock guarantees initial discovery/publication/GC has finished.
	s.mu.Lock()
	s.mu.Unlock()
	f.reset()
}

func waitRevision(t *testing.T, cat *deploy.Catalog, site, revision string) {
	t.Helper()
	waitTarget(t, func() bool {
		s, err := cat.GetSite(context.Background(), site)
		if err != nil {
			return false
		}
		r, err := cat.GetRelease(context.Background(), s.Owner, s.Name, s.Production)
		return err == nil && r.Origin.Revision == revision
	})
}

func assertOnlyTarget(t *testing.T, f *targetedForge, store *targetedStore, name string) {
	t.Helper()
	prefix := "/repos/acme/" + name + ".app"
	for _, path := range f.paths() {
		if path != prefix && !strings.HasPrefix(path, prefix+"/") {
			t.Fatalf("targeted event accessed another source path: %s", path)
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, key := range append(append([]string(nil), store.writes...), store.deletes...) {
		if strings.HasPrefix(key, "sites/") && key != deploy.SiteKey(name) || strings.HasPrefix(key, "sources/") && !strings.Contains(key, "/sites/"+name+"/") {
			t.Fatalf("targeted event changed another site's data: %s", key)
		}
	}
}

func TestRepositoryNotificationDoesNotReadPublishOrCollectOtherRepository(t *testing.T) {
	f, server := targetedFixture(t)
	store := &targetedStore{Store: storage.NewMemory()}
	s, cat := targetedSource(t, server.URL, store)
	startTargeted(t, s, f)
	// An unrelated retired release is eligible for GC; a whole-source pass
	// would collect it. A notification for foo must leave it and bar untouched.
	bar, err := cat.GetSite(context.Background(), "bar")
	if err != nil {
		t.Fatal(err)
	}
	orphan, err := cat.Publish(context.Background(), "bar", "source", deploy.Origin{Kind: "git", RepositoryID: "43", Repository: "acme/bar.app", Revision: sha(90), Directory: "public"}, []deploy.InputFile{{Path: "index.html", Size: 6, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("orphan")), nil }}})
	if err != nil {
		t.Fatal(err)
	}
	bar.Retired[orphan.ID] = time.Now().Add(-time.Hour)
	if err := cat.SaveSite(context.Background(), bar); err != nil {
		t.Fatal(err)
	}
	// A separate process has written intent for bar. It supplies no local
	// control wakeup, and a foo webhook must not discover or apply that intent.
	otherProcess := deploy.New(store)
	if _, err := otherProcess.RequestControl(context.Background(), "bar", "source", "disabled", ""); err != nil {
		t.Fatal(err)
	}
	_, before, err := cat.GetSiteInfo(context.Background(), "bar")
	if err != nil {
		t.Fatal(err)
	}
	store.reset()
	f.advance()
	if !s.NotifyRepository("acme/foo.app") {
		t.Fatal("target ignored")
	}
	waitRevision(t, cat, "foo", sha(3))
	s.mu.Lock()
	s.mu.Unlock()
	assertOnlyTarget(t, f, store, "foo")
	_, after, err := cat.GetSiteInfo(context.Background(), "bar")
	if err != nil || before.ETag != after.ETag {
		t.Fatalf("unrelated publication changed: %v", err)
	}
	if _, err := cat.GetRelease(context.Background(), "source", "bar", orphan.ID); err != nil {
		t.Fatalf("unrelated release was garbage-collected: %v", err)
	}
}

func TestRepositoryNotificationsKeepDistinctAndInflightHints(t *testing.T) {
	f, server := targetedFixture(t)
	s, cat := targetedSource(t, server.URL, storage.NewMemory())
	startTargeted(t, s, f)
	f.advance()
	entered, release := make(chan struct{}), make(chan struct{})
	f.mu.Lock()
	f.gatePath, f.entered, f.release = "/repos/acme/foo.app", entered, release
	f.mu.Unlock()
	s.NotifyRepository("acme/foo.app")
	awaitNotification(t, entered)
	for i := 0; i < 1000; i++ {
		s.NotifyRepository("acme/foo.app")
		s.NotifyRepository("acme/bar.app")
	}
	close(release)
	waitRevision(t, cat, "bar", sha(3))
	waitTarget(t, func() bool {
		count := 0
		for _, path := range f.paths() {
			if path == "/repos/acme/foo.app" {
				count++
			}
		}
		return count == 2
	})
	time.Sleep(2 * notificationDebounce)
	counts := map[string]int{}
	for _, path := range f.paths() {
		if path == "/orgs/acme/repos" {
			t.Fatal("repository hints triggered owner discovery")
		}
		counts[path]++
	}
	if counts["/repos/acme/foo.app"] != 2 || counts["/repos/acme/bar.app"] != 1 {
		t.Fatalf("hints lost or not coalesced: %v", counts)
	}
}

func TestRepositoryNotificationDiscoversNewRepositoryWithoutListing(t *testing.T) {
	f, server := targetedFixture(t)
	f.repos = map[string]repository{}
	store := &targetedStore{Store: storage.NewMemory()}
	s, cat := targetedSource(t, server.URL, store)
	startTargeted(t, s, f)
	f.mu.Lock()
	f.repos["acme/foo.app"] = repository{ID: 42, Name: "foo.app", FullName: "acme/foo.app", DefaultBranch: "main"}
	f.mu.Unlock()
	s.NotifyRepository("acme/foo.app")
	waitRevision(t, cat, "foo", sha(2))
	assertOnlyTarget(t, f, store, "foo")
}

func TestRepositoryNotificationFailureKeepsPublicationAndScopedRetry(t *testing.T) {
	f, server := targetedFixture(t)
	store := &targetedStore{Store: storage.NewMemory()}
	s, cat := targetedSource(t, server.URL, store)
	startTargeted(t, s, f)
	before, _, err := cat.GetSiteInfo(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	f.advance()
	f.mu.Lock()
	f.failures["/repos/acme/foo.app"] = 1
	f.mu.Unlock()
	store.reset()
	s.NotifyRepository("acme/foo.app")
	waitTarget(t, func() bool { return len(f.paths()) == 1 })
	current, err := cat.GetSite(context.Background(), "foo")
	if err != nil || current.Production != before.Production {
		t.Fatalf("failed target metadata changed publication: %v", err)
	}
	waitRevision(t, cat, "foo", sha(3))
	s.mu.Lock()
	s.mu.Unlock()
	assertOnlyTarget(t, f, store, "foo")
	metadataCalls := 0
	for _, path := range f.paths() {
		if path == "/repos/acme/foo.app" {
			metadataCalls++
		}
	}
	if metadataCalls != 2 {
		t.Fatalf("failed repository job did not retry independently: %v", f.paths())
	}
}

func TestTargetedRepositoryRejectsMissingChangedAndMismatchedIdentity(t *testing.T) {
	f, server := targetedFixture(t)
	store := &targetedStore{Store: storage.NewMemory()}
	s, cat := targetedSource(t, server.URL, store)
	if err := s.syncNotifiedRepository(context.Background(), "acme/foo.app"); err != nil {
		t.Fatal(err)
	}
	_, before, err := cat.GetSiteInfo(context.Background(), "foo")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		status int
		repo   repository
	}{
		{"missing", 404, repository{}},
		{"forbidden", 403, repository{}},
		{"renamed redirect", 0, repository{ID: 42, Name: "bar.app", FullName: "acme/bar.app", DefaultBranch: "main"}},
		{"transferred", 0, repository{ID: 42, Name: "foo.app", FullName: "other/foo.app", DefaultBranch: "main"}},
		{"name mismatch", 0, repository{ID: 42, Name: "bar.app", FullName: "acme/foo.app", DefaultBranch: "main"}},
		{"missing ID", 0, repository{Name: "foo.app", FullName: "acme/foo.app", DefaultBranch: "main"}},
		{"recreated repository", 0, repository{ID: 99, Name: "foo.app", FullName: "acme/foo.app", DefaultBranch: "main"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.mu.Lock()
			f.statuses["/repos/acme/foo.app"] = tc.status
			f.overrides["acme/foo.app"] = tc.repo
			f.mu.Unlock()
			f.reset()
			store.reset()
			if err := s.syncNotifiedRepository(context.Background(), "acme/foo.app"); err == nil {
				t.Fatal("accepted invalid target identity")
			}
			_, after, err := cat.GetSiteInfo(context.Background(), "foo")
			if err != nil || before.ETag != after.ETag {
				t.Fatalf("invalid identity changed publication: %v", err)
			}
			store.mu.Lock()
			if len(store.writes)+len(store.deletes) != 0 {
				t.Errorf("invalid target identity wrote or deleted objects")
			}
			store.mu.Unlock()
			if paths := f.paths(); len(paths) != 1 || paths[0] != "/repos/acme/foo.app" {
				t.Fatalf("invalid identity accessed unrelated API: %v", paths)
			}
		})
	}
}

func TestTargetedRepositoryCannotReplaceExistingSiteWithCollidingRepository(t *testing.T) {
	f, server := targetedFixture(t)
	cat := deploy.New(storage.NewMemory())
	s, err := New(cat, Config{ID: "source", Owner: "acme", APIURL: server.URL, RepositoryPattern: `^([a-z]+)(?:-alias)?\.app$`, Branches: []string{"main"}, RecentCommits: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.syncNotifiedRepository(context.Background(), "acme/foo.app"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.repos["acme/foo-alias.app"] = repository{ID: 44, Name: "foo-alias.app", FullName: "acme/foo-alias.app", DefaultBranch: "main"}
	f.mu.Unlock()
	if err := s.syncNotifiedRepository(context.Background(), "acme/foo-alias.app"); !errors.Is(err, deploy.ErrOwnership) {
		t.Fatalf("colliding repository replaced a stable site: %v", err)
	}
}
