package githubsync

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/serving"
	"github.com/lesomnus/page/internal/storage"
	"gopkg.in/yaml.v3"
)

func TestDeploymentManifest(t *testing.T) {
	const current = "schemaVersion: 1\npublish:\n  directory: .\n"
	const legacy = `{"schemaVersion":1,"publish":{"directory":"public"}}`
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
		{name: "YAML wins over legacy", files: map[string]string{"page.yaml": current, "manifest.json": legacy}, directory: ".", want: []string{"page.yaml"}},
		{name: "immutable legacy commit", files: map[string]string{"manifest.json": legacy}, directory: "public", want: []string{"page.yaml", "manifest.json"}},
		{name: "explicit former path", configured: "manifest.json", files: map[string]string{"manifest.json": legacy}, directory: "public", want: []string{"manifest.json"}},
		{name: "custom YAML path", configured: "deploy/site.yaml", files: map[string]string{"deploy/site.yaml": current}, directory: ".", want: []string{"deploy/site.yaml"}},
		{name: "custom path does not fall back", configured: "deploy/site.yaml", files: map[string]string{"manifest.json": legacy}, missing: true, want: []string{"deploy/site.yaml"}},
		{name: "both missing", missing: true, want: []string{"page.yaml", "manifest.json"}},
		{name: "malformed YAML never falls back", files: map[string]string{"page.yaml": "publish: [", "manifest.json": legacy}, want: []string{"page.yaml"}},
		{name: "unknown field", files: map[string]string{"page.yaml": current + "unexpected: true\n", "manifest.json": legacy}, want: []string{"page.yaml"}},
		{name: "duplicate field", files: map[string]string{"page.yaml": current + "schemaVersion: 1\n"}, want: []string{"page.yaml"}},
		{name: "multiple documents", files: map[string]string{"page.yaml": current + "---\n" + current}, want: []string{"page.yaml"}},
		{name: "fractional schema", files: map[string]string{"page.yaml": "schemaVersion: 1.5\npublish:\n  directory: public\n"}, want: []string{"page.yaml"}},
		{name: "quoted schema", files: map[string]string{"page.yaml": "schemaVersion: \"1\"\npublish:\n  directory: public\n"}, want: []string{"page.yaml"}},
		{name: "wrong schema", files: map[string]string{"page.yaml": "schemaVersion: 2\npublish:\n  directory: public\n", "manifest.json": legacy}, want: []string{"page.yaml"}},
		{name: "unsafe directory", files: map[string]string{"page.yaml": "schemaVersion: 1\npublish:\n  directory: ../private\n", "manifest.json": legacy}, want: []string{"page.yaml"}},
		{name: "empty file", files: map[string]string{"page.yaml": "", "manifest.json": legacy}, want: []string{"page.yaml"}},
		{name: "oversized file", files: map[string]string{"page.yaml": current + "#" + strings.Repeat("x", 65536), "manifest.json": legacy}, want: []string{"page.yaml"}},
		{name: "upstream error never falls back", status: http.StatusBadGateway, files: map[string]string{"manifest.json": legacy}, want: []string{"page.yaml"}},
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

func TestYAMLProductionRetainsLegacyCommitPreview(t *testing.T) {
	f, api := fixture(t)
	f.mu.Lock()
	delete(f.manifests, sha(1))
	f.legacyManifests = map[string]string{sha(1): `{"schemaVersion":1,"publish":{"directory":"public"}}`}
	f.mu.Unlock()
	cat := deploy.New(storage.NewMemory())
	s := newSync(t, cat, config(api.URL))
	mustOnce(t, s)
	site := getSite(t, cat)
	alias, ok := site.Aliases[deploy.AliasToken("commit", sha(1))]
	if !ok {
		t.Fatal("switching to page.yaml lost a retained historical commit preview")
	}
	release, err := cat.GetRelease(context.Background(), site.Owner, site.Name, alias.Release)
	if err != nil || release.Origin.Revision != sha(1) {
		t.Fatalf("wrong historical release: %v", err)
	}
	h, err := serving.New(cat, serving.Config{BaseDomain: "example.com", PreviewStyle: "flat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := h.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertHTTP(t, h, "foo.example.com", http.StatusOK, "snapshot-"+sha(2))
	assertHTTP(t, h, deploy.Host("example.com", "flat", "foo", deploy.AliasToken("commit", sha(1))), http.StatusOK, "snapshot-"+sha(1))
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
