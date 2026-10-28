package deploy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

type projectionStore struct {
	storage.Store
	puts, gets, stats, deletes []string
	failPut, failDelete        string
	maxRead, bytesRead         int
	ignoreManifestSize         bool
}

func (s *projectionStore) Put(ctx context.Context, key string, body io.Reader, size int64, typ string) (storage.Info, error) {
	s.puts = append(s.puts, key)
	if s.failPut != "" && strings.Contains(key, s.failPut) {
		return storage.Info{}, errors.New("projection write interrupted")
	}
	return s.Store.Put(ctx, key, body, size, typ)
}

func (s *projectionStore) Get(ctx context.Context, key string) (io.ReadCloser, storage.Info, error) {
	s.gets = append(s.gets, key)
	body, info, err := s.Store.Get(ctx, key)
	if err != nil {
		return nil, info, err
	}
	if strings.HasSuffix(key, "/manifest.json") {
		if s.ignoreManifestSize {
			info.Size = 0
		}
		body = &projectionReadCloser{ReadCloser: body, store: s}
	}
	return body, info, nil
}

func (s *projectionStore) Stat(ctx context.Context, key string) (storage.Info, error) {
	s.stats = append(s.stats, key)
	info, err := s.Store.Stat(ctx, key)
	if s.ignoreManifestSize && strings.HasSuffix(key, "/manifest.json") {
		info.Size = 0
	}
	return info, err
}

func (s *projectionStore) Delete(ctx context.Context, key string) error {
	s.deletes = append(s.deletes, key)
	if s.failDelete != "" && strings.Contains(key, s.failDelete) {
		return errors.New("projection deletion interrupted")
	}
	return s.Store.Delete(ctx, key)
}

type projectionReadCloser struct {
	io.ReadCloser
	store *projectionStore
}

func (r *projectionReadCloser) Read(p []byte) (int, error) {
	r.store.maxRead = max(r.store.maxRead, len(p))
	n, err := r.ReadCloser.Read(p)
	r.store.bytesRead += n
	return n, err
}

func newProjectionCatalog() (*Catalog, *projectionStore) {
	s := &projectionStore{Store: storage.NewMemory()}
	return New(s), s
}

func projectFiles() []InputFile {
	return []InputFile{
		input("index.html", "<p>page</p>"),
		input("assets/app.abc.js", "alert(1)"),
		input("pages.json", `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`),
	}
}

func TestServingProjectionPublicationAndPointReads(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("projection"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	prefix := ReleasePrefix(r.Owner, r.Site, r.ID)
	stage := 0
	metadata := 0
	for _, key := range store.puts {
		next := 0
		switch {
		case key == prefix+"upload.json":
			next = 0
		case strings.HasPrefix(key, prefix+"files/"):
			next = 1
		case strings.HasPrefix(key, prefix+"metadata/"):
			next = 2
			metadata++
		case key == prefix+"serving.json":
			next = 3
		case key == prefix+"manifest.json":
			next = 4
		default:
			t.Fatalf("unexpected publication object: %s", key)
		}
		if next < stage {
			t.Fatalf("publication order regressed at %s", key)
		}
		stage = next
	}
	if stage != 4 || metadata != len(r.Files) {
		t.Fatalf("incomplete projection: stage=%d metadata=%d", stage, metadata)
	}
	store.gets, store.stats = nil, nil
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.stats) != 1 || store.stats[0] != prefix+"manifest.json" || len(store.gets) != 1 || store.gets[0] != prefix+"serving.json" {
		t.Fatalf("serving release read inventory: gets=%v stats=%v", store.gets, store.stats)
	}
	encoded, err := json.Marshal(header)
	if err != nil || bytes.Contains(encoded, []byte(`"files"`)) || bytes.Contains(encoded, []byte("assets/app.abc.js")) {
		t.Fatalf("file inventory leaked into serving descriptor: %s (%v)", encoded, err)
	}
	store.gets = nil
	f, err := c.GetServingFile(ctx, header, "assets/app.abc.js")
	if err != nil || f.File != r.Files["assets/app.abc.js"] || !f.Immutable {
		t.Fatalf("wrong point metadata: %#v %v", f, err)
	}
	if len(store.gets) != 1 || store.gets[0] != ServingFileKey(header, f.File.Path) {
		t.Fatalf("file lookup was not a point read: %v", store.gets)
	}
	store.gets = nil
	if _, err := c.GetServingFile(ctx, header, "missing.js"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing metadata: %v", err)
	}
	if len(store.gets) != 1 || store.gets[0] != ServingFileKey(header, "missing.js") {
		t.Fatalf("missing metadata fell back to inventory: %v", store.gets)
	}
	metadataName := strings.TrimPrefix(ServingFileKey(header, "a/b"), prefix+"metadata/")
	if len(metadataName) != 64+len(".json") || strings.Contains(metadataName, "/") || ServingFileKey(header, "a") == ServingFileKey(header, "a/b") {
		t.Fatalf("metadata keys do not hash the full path: %s", metadataName)
	}
}

func TestServingProjectionRejectsUnpublishedOrphans(t *testing.T) {
	ctx := context.Background()
	for _, failure := range []string{"/metadata/", "/serving.json", "/manifest.json"} {
		t.Run(failure, func(t *testing.T) {
			c, store := newProjectionCatalog()
			store.failPut = failure
			if _, err := c.Publish(ctx, "foo", "source", origin("failed"), projectFiles()); err == nil {
				t.Fatal("interrupted projection was published")
			}
			id := ReleaseID("source", "foo", origin("failed"))
			if _, err := c.GetServingRelease(ctx, "source", "foo", id); !errors.Is(err, storage.ErrNotFound) {
				t.Fatalf("unpublished projection became readable: %v", err)
			}
			store.failPut = ""
			store.puts = nil
			r, err := c.Publish(ctx, "foo", "source", origin("failed"), projectFiles())
			if err != nil {
				t.Fatal(err)
			}
			writes := 0
			for _, key := range store.puts {
				if strings.Contains(key, "/metadata/") {
					writes++
				}
			}
			if writes != len(r.Files) {
				t.Fatalf("retry reused an orphaned projection: metadata writes=%d", writes)
			}
			header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetServingFile(ctx, header, "assets/app.abc.js"); err != nil {
				t.Fatalf("retry did not rebuild metadata: %v", err)
			}
		})
	}
}

func TestServingProjectionValidatesFileEnvelopeAndOriginalPath(t *testing.T) {
	ctx := context.Background()
	c, _ := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("identity"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	good, err := c.GetServingFile(ctx, header, "assets/app.abc.js")
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ServingFile){
		"schema":     func(f *ServingFile) { f.SchemaVersion++ },
		"owner":      func(f *ServingFile) { f.Owner = "foreign" },
		"site":       func(f *ServingFile) { f.Site = "foreign" },
		"release":    func(f *ServingFile) { f.ReleaseID = strings.Repeat("0", 64) },
		"repository": func(f *ServingFile) { f.RepositoryID = "foreign" },
		"created":    func(f *ServingFile) { f.CreatedAt = f.CreatedAt.Add(time.Second) },
		"path":       func(f *ServingFile) { f.File.Path = "assets/other.js" },
		"digest":     func(f *ServingFile) { f.File.SHA256 = strings.Repeat("z", 64) },
		"html":       func(f *ServingFile) { f.File.ContentType = "text/html" },
	} {
		t.Run(name, func(t *testing.T) {
			bad := *good
			change(&bad)
			if err := c.write(ctx, ServingFileKey(header, good.File.Path), &bad); err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetServingFile(ctx, header, good.File.Path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted foreign or corrupt metadata: %v", err)
			}
		})
	}
}

func removeProjection(t *testing.T, ctx context.Context, c *Catalog, r *Release) {
	t.Helper()
	header := servingRelease(r)
	if err := c.Store.Delete(ctx, ServingReleaseKey(header)); err != nil {
		t.Fatal(err)
	}
	for p := range r.Files {
		if err := c.Store.Delete(ctx, ServingFileKey(header, p)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestLegacyServingAndPublisherBackfill(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("legacy"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	removeProjection(t, ctx, c, r)
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil || header.FileMetadataVersion != 0 {
		t.Fatalf("legacy release is unreadable: %#v %v", header, err)
	}
	f, err := c.GetServingFile(ctx, header, "assets/app.abc.js")
	if err != nil || !f.Immutable || f.File != r.Files["assets/app.abc.js"] {
		t.Fatalf("legacy file or immutable policy lost: %#v %v", f, err)
	}
	if _, err := c.GetServingFile(ctx, header, "missing.js"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing legacy file: %v", err)
	}
	store.puts = nil
	files := projectFiles()
	for i := range files {
		files[i].Open = func() (io.ReadCloser, error) { return nil, errors.New("must reuse completed content") }
	}
	again, err := c.Publish(ctx, r.Site, r.Owner, r.Origin, files)
	if err != nil || !again.CreatedAt.Equal(r.CreatedAt) {
		t.Fatalf("backfill did not reuse completed release: %#v %v", again, err)
	}
	for _, key := range store.puts {
		if strings.Contains(key, "/files/") || strings.HasSuffix(key, "/manifest.json") {
			t.Fatalf("backfill rewrote completed content: %s", key)
		}
	}
	header, err = c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil || header.FileMetadataVersion != fileMetadataVersion {
		t.Fatalf("publisher did not backfill projection: %#v %v", header, err)
	}
	if _, err := c.GetServingFile(ctx, header, "assets/app.abc.js"); err != nil {
		t.Fatal(err)
	}
}

func TestServingProjectionGCWithdrawsBothCompletionObjectsFirst(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("gc"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	s := siteFor(r)
	s.Production = ""
	if err := c.SaveSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	store.deletes = nil
	store.failDelete = "/files/"
	if err := c.DeleteRelease(ctx, s, r.ID); err == nil {
		t.Fatal("expected interrupted GC")
	}
	prefix := ReleasePrefix(r.Owner, r.Site, r.ID)
	if len(store.deletes) < 3 || store.deletes[0] != prefix+"serving.json" || store.deletes[1] != prefix+"manifest.json" {
		t.Fatalf("GC deleted data before withdrawing both completion objects: %v", store.deletes)
	}
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("interrupted GC still serves release: %v", err)
	}
	if _, err := store.Stat(ctx, prefix+"upload.json"); err != nil {
		t.Fatalf("GC lost recovery marker: %v", err)
	}
	store.failDelete = ""
	if err := c.DeleteRelease(ctx, s, r.ID); err != nil {
		t.Fatal(err)
	}
	if store.deletes[len(store.deletes)-1] != prefix+"upload.json" {
		t.Fatalf("upload marker was not deleted last: %v", store.deletes)
	}
}

func TestMissingManifestForcesProjectionRebuild(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("reselected"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	header := servingRelease(r)
	if err := store.Delete(ctx, ReleasePrefix(r.Owner, r.Site, r.ID)+"manifest.json"); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, ServingFileKey(header, "assets/app.abc.js")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("orphaned serving descriptor remained visible: %v", err)
	}
	store.puts = nil
	r, err = c.Publish(ctx, r.Site, r.Owner, r.Origin, projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	metadata := 0
	for _, key := range store.puts {
		if strings.Contains(key, "/metadata/") {
			metadata++
		}
	}
	if metadata != len(r.Files) {
		t.Fatalf("orphaned descriptor prevented complete rebuild: %v", store.puts)
	}
	header, err = c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServingFile(ctx, header, "assets/app.abc.js"); err != nil {
		t.Fatal(err)
	}
}

func TestLegacyServingStreamsLargeInventoryWithBoundedReadBuffer(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	o := origin("large-legacy")
	r := &Release{SchemaVersion: SchemaVersion, ID: ReleaseID("source", "foo", o), Site: "foo", Owner: "source", Origin: o, CreatedAt: time.Now().UTC(), Complete: true, Files: map[string]File{}}
	for i := range 20000 {
		p := fmt.Sprintf("assets/%06d.js", i)
		r.Files[p] = File{Path: p, Size: 1, ContentType: "application/javascript", SHA256: strings.Repeat("a", 64)}
	}
	if err := c.write(ctx, ReleasePrefix(r.Owner, r.Site, r.ID)+"manifest.json", r); err != nil {
		t.Fatal(err)
	}
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	f, err := c.GetServingFile(ctx, header, "assets/019999.js")
	if err != nil || f.File != r.Files["assets/019999.js"] {
		t.Fatalf("legacy point lookup lost record: %#v %v", f, err)
	}
	if store.maxRead > 64<<10 || store.bytesRead < 2<<20 {
		t.Fatalf("inventory was not streamed with a bounded buffer: max read=%d total=%d", store.maxRead, store.bytesRead)
	}
}

func TestLegacyServingEnforcesActualMetadataLimit(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("large-tail"), []InputFile{input("index.html", "x")})
	if err != nil {
		t.Fatal(err)
	}
	removeProjection(t, ctx, c, r)
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	b = append(b, bytes.Repeat([]byte(" "), maxMetadata+1024)...)
	key := ReleasePrefix(r.Owner, r.Site, r.ID) + "manifest.json"
	if _, err := store.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "application/json"); err != nil {
		t.Fatal(err)
	}
	store.ignoreManifestSize = true
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized actual metadata accepted: %v", err)
	}
	if store.bytesRead > maxMetadata+1 {
		t.Fatalf("legacy reader exceeded byte limit: %d", store.bytesRead)
	}
}

func TestLegacyServingLoadsAndVerifiesEmbeddedPagesFile(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("old-pages"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	removeProjection(t, ctx, c, r)
	r.Pages = nil
	key := ReleasePrefix(r.Owner, r.Site, r.ID) + "manifest.json"
	if err := c.write(ctx, key, r); err != nil {
		t.Fatal(err)
	}
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil || header.Pages == nil {
		t.Fatalf("legacy pages.json was not loaded: %#v %v", header, err)
	}
	f, err := c.GetServingFile(ctx, header, "assets/app.abc.js")
	if err != nil || !f.Immutable {
		t.Fatalf("legacy immutable policy was not loaded: %#v %v", f, err)
	}
	if _, err := store.Put(ctx, FileKey(r, "pages.json"), strings.NewReader("{}"), 2, "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("legacy pages digest mismatch accepted: %v", err)
	}
	r.Owner = "foreign"
	if err := c.write(ctx, key, r); err != nil {
		t.Fatal(err)
	}
	store.gets = nil
	if _, err := c.GetServingRelease(ctx, "source", "foo", r.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("foreign legacy header accepted: %v", err)
	}
	for _, key := range store.gets {
		if strings.Contains(key, "/files/") {
			t.Fatalf("invalid envelope triggered an owned file read: %s", key)
		}
	}
}
