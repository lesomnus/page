package githubsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
	"gopkg.in/yaml.v3"
)

func TestDeploymentManifest(t *testing.T) {
	const current = "schemaVersion: 1\npublish:\n  directory: .\n"
	const jsonManifest = `{"schemaVersion":1,"publish":{"directory":"public"}}`
	for _, tt := range []struct {
		name       string
		configured string
		files      map[string]string
		status     int
		directory  string
		want       []string
		missing    bool
	}{
		{name: "YAML with comments", files: map[string]string{"page.yaml": "# site configuration\n" + current}, directory: ".", want: []string{"page.yaml"}},
		{name: "configured YAML path wins", files: map[string]string{"page.yaml": current, "manifest.json": jsonManifest}, directory: ".", want: []string{"page.yaml"}},
		{name: "missing page.yaml ignores unconfigured JSON", files: map[string]string{"manifest.json": jsonManifest}, missing: true, want: []string{"page.yaml"}},
		{name: "explicit configured JSON path", configured: "manifest.json", files: map[string]string{"manifest.json": jsonManifest}, directory: "public", want: []string{"manifest.json"}},
		{name: "custom YAML path", configured: "deploy/site.yaml", files: map[string]string{"deploy/site.yaml": current}, directory: ".", want: []string{"deploy/site.yaml"}},
		{name: "custom path does not fall back", configured: "deploy/site.yaml", files: map[string]string{"manifest.json": jsonManifest}, missing: true, want: []string{"deploy/site.yaml"}},
		{name: "configured file missing", missing: true, want: []string{"page.yaml"}},
		{name: "malformed YAML never falls back", files: map[string]string{"page.yaml": "publish: [", "manifest.json": jsonManifest}, want: []string{"page.yaml"}},
		{name: "unknown field", files: map[string]string{"page.yaml": current + "unexpected: true\n", "manifest.json": jsonManifest}, want: []string{"page.yaml"}},
		{name: "duplicate field", files: map[string]string{"page.yaml": current + "schemaVersion: 1\n"}, want: []string{"page.yaml"}},
		{name: "multiple documents", files: map[string]string{"page.yaml": current + "---\n" + current}, want: []string{"page.yaml"}},
		{name: "fractional schema", files: map[string]string{"page.yaml": "schemaVersion: 1.5\npublish:\n  directory: public\n"}, want: []string{"page.yaml"}},
		{name: "quoted schema", files: map[string]string{"page.yaml": "schemaVersion: \"1\"\npublish:\n  directory: public\n"}, want: []string{"page.yaml"}},
		{name: "wrong schema", files: map[string]string{"page.yaml": "schemaVersion: 2\npublish:\n  directory: public\n", "manifest.json": jsonManifest}, want: []string{"page.yaml"}},
		{name: "unsafe directory", files: map[string]string{"page.yaml": "schemaVersion: 1\npublish:\n  directory: ../private\n", "manifest.json": jsonManifest}, want: []string{"page.yaml"}},
		{name: "empty file", files: map[string]string{"page.yaml": "", "manifest.json": jsonManifest}, want: []string{"page.yaml"}},
		{name: "oversized file", files: map[string]string{"page.yaml": current + "#" + strings.Repeat("x", 65536), "manifest.json": jsonManifest}, want: []string{"page.yaml"}},
		{name: "upstream error never falls back", status: http.StatusBadGateway, files: map[string]string{"manifest.json": jsonManifest}, want: []string{"page.yaml"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var read []string
			var mu sync.Mutex
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				filename := strings.TrimPrefix(r.URL.Path, "/repos/acme/foo.app/contents/")
				mu.Lock()
				read = append(read, filename)
				mu.Unlock()
				if r.URL.Query().Get("ref") != sha(2) {
					t.Error("manifest was not read at the requested immutable SHA")
				}
				if tt.status != 0 {
					w.WriteHeader(tt.status)
				} else if data, ok := tt.files[filename]; ok {
					w.Write([]byte(data))
				} else {
					http.NotFound(w, r)
				}
			}))
			defer api.Close()
			cfg := config(api.URL)
			cfg.ManifestPath = tt.configured
			s := newSync(t, deploy.New(storage.NewMemory()), cfg)
			if tt.configured == "" && s.cfg.ManifestPath != "page.yaml" {
				t.Fatal("default deployment file is not page.yaml")
			}
			m, err := s.deploymentManifest(context.Background(), repository{FullName: "acme/foo.app"}, sha(2))
			if tt.directory != "" {
				if err != nil || m.Publish.Directory != tt.directory {
					t.Fatalf("got directory %q, error %v", m.Publish.Directory, err)
				}
			} else if err == nil || (tt.missing && !errors.Is(err, errNoManifest)) {
				t.Fatalf("expected a rejected or absent manifest, got %v", err)
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(read, tt.want) {
				t.Fatalf("requested %v, want %v", read, tt.want)
			}
		})
	}
}

func TestHistoricalRefsWithoutPageYAMLAreWithdrawn(t *testing.T) {
	ctx := context.Background()
	forge, _ := fixture(t)
	var unconfiguredReads atomic.Int64
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/acme/foo.app/contents/manifest.json" {
			unconfiguredReads.Add(1)
			w.Write([]byte(`{"schemaVersion":1,"publish":{"directory":"public"}}`))
			return
		}
		forge.ServeHTTP(w, r)
	}))
	defer api.Close()
	forge.mu.Lock()
	forge.branches["historic"] = sha(1)
	forge.mu.Unlock()
	cat := deploy.New(storage.NewMemory())
	cfg := config(api.URL)
	cfg.GCGrace = 24 * time.Hour
	syncer := newSync(t, cat, cfg)
	mustOnce(t, syncer)
	before := getSite(t, cat)
	tokens := []string{
		deploy.AliasToken("branch", "refs/heads/historic"),
		deploy.AliasToken("tag", "refs/tags/v1.0"),
		deploy.AliasToken("commit", sha(1)),
	}
	for _, token := range tokens {
		if before.Aliases[token].Revision != sha(1) {
			t.Fatalf("fixture did not publish historical ref %s", token)
		}
	}
	oldRelease := before.Aliases[tokens[0]].Release
	// This revision now has only the unconfigured manifest.json above. The
	// current default branch still has page.yaml and remains deployable.
	forge.mu.Lock()
	delete(forge.manifests, sha(1))
	forge.mu.Unlock()
	mustOnce(t, syncer)
	after := getSite(t, cat)
	if after.Production != before.Production {
		t.Fatal("historical manifest exclusion changed current production")
	}
	for _, token := range tokens {
		if _, exists := after.Aliases[token]; exists {
			t.Fatalf("unsupported historical ref %s stayed published", token)
		}
	}
	if after.Retired[oldRelease].IsZero() {
		t.Fatal("withdrawn historical release was not retired for source-owned GC")
	}
	if _, err := cat.GetRelease(ctx, after.Owner, after.Name, oldRelease); err != nil {
		t.Fatal("manifest exclusion deleted storage before GC grace", err)
	}
	if unconfiguredReads.Load() != 0 {
		t.Fatal("source attempted an unconfigured manifest.json fallback")
	}
	h, err := serving.New(cat, serving.Config{BaseDomain: "example.com", PreviewStyle: "flat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	assertHTTP(t, h, "foo.example.com", http.StatusOK, "snapshot-"+sha(2))
	for _, token := range tokens {
		assertHTTP(t, h, deploy.Host("example.com", "flat", "foo", token), http.StatusNotFound, "")
	}
}

func TestManifestDecodeErrorDoesNotExposePrivateScalars(t *testing.T) {
	const secret = "pwsecret"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("schemaVersion: 1\npublish: " + secret + "\n"))
	}))
	defer api.Close()
	s := newSync(t, deploy.New(storage.NewMemory()), config(api.URL))
	_, err := s.deploymentManifest(context.Background(), repository{FullName: "acme/foo.app"}, sha(2))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("private YAML contents escaped into the rendered diagnostic: %v", err)
	}
	var typed *yaml.TypeError
	if !errors.As(err, &typed) {
		t.Fatal("YAML diagnostic cause was lost")
	}
}
