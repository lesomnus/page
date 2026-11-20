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
	return body, info, nil
}

func (s *projectionStore) Stat(ctx context.Context, key string) (storage.Info, error) {
	s.stats = append(s.stats, key)
	info, err := s.Store.Stat(ctx, key)
	return info, err
}

func (s *projectionStore) Delete(ctx context.Context, key string) error {
	s.deletes = append(s.deletes, key)
	if s.failDelete != "" && strings.Contains(key, s.failDelete) {
		return errors.New("projection deletion interrupted")
	}
	return s.Store.Delete(ctx, key)
}

// shardCount is how many index objects publishing r writes.
func shardCount(r *Release) int {
	shards := map[string]bool{}
	for p := range r.Files {
		shards[IndexShard(p)] = true
	}
	return len(shards)
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
	blobs := blobPrefix(r.Owner, r.Site)
	stage := 0
	shards := map[string]bool{}
	for p := range r.Files {
		shards[IndexShard(p)] = true
	}
	index := 0
	for _, key := range store.puts {
		next := 0
		switch {
		case key == prefix+uploadObject:
			next = 0
		case strings.HasPrefix(key, blobs):
			next = 1
		case strings.HasPrefix(key, prefix+indexDir):
			next = 2
			index++
		case key == prefix+servingObject:
			next = 3
		case key == prefix+releaseObject:
			next = 4
		default:
			t.Fatalf("unexpected publication object: %s", key)
		}
		if next < stage {
			t.Fatalf("publication order regressed at %s", key)
		}
		stage = next
	}
	if stage != 4 || index != len(shards) {
		t.Fatalf("incomplete projection: stage=%d index=%d shards=%d", stage, index, len(shards))
	}
	store.gets, store.stats = nil, nil
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.stats) != 1 || store.stats[0] != prefix+releaseObject || len(store.gets) != 1 || store.gets[0] != prefix+servingObject {
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
	if len(store.gets) != 1 || store.gets[0] != ServingIndexKey(header, IndexShard(f.File.Path)) {
		t.Fatalf("file lookup was not a single shard read: %v", store.gets)
	}
	store.gets = nil
	if _, err := c.GetServingFile(ctx, header, "missing.js"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("missing metadata: %v", err)
	}
	if len(store.gets) != 1 || store.gets[0] != ServingIndexKey(header, IndexShard("missing.js")) {
		t.Fatalf("missing metadata fell back to inventory: %v", store.gets)
	}
	shardName := strings.TrimPrefix(ServingIndexKey(header, IndexShard("a/b")), prefix+indexDir)
	if len(shardName) != 2+len(".json") || strings.Contains(shardName, "/") {
		t.Fatalf("index keys do not hash the full path: %s", shardName)
	}
}

func TestPublicationStoresUnchangedBodiesOnce(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	first, err := c.Publish(ctx, "foo", "source", origin("one"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	store.puts = nil
	files := projectFiles()
	files[0] = input("index.html", "<p>changed</p>")
	second, err := c.Publish(ctx, "foo", "source", origin("two"), files)
	if err != nil {
		t.Fatal(err)
	}
	blobs := blobPrefix(second.Owner, second.Site)
	var written []string
	for _, key := range store.puts {
		if strings.HasPrefix(key, blobs) {
			written = append(written, key)
		}
	}
	if len(written) != 1 || written[0] != BlobKey(second.Owner, second.Site, second.Files["index.html"].SHA256) {
		t.Fatalf("unchanged bodies uploaded again: %v", written)
	}
	if first.Files["assets/app.abc.js"].SHA256 != second.Files["assets/app.abc.js"].SHA256 {
		t.Fatal("same body got a different digest")
	}
}

func TestServingProjectionRejectsUnpublishedOrphans(t *testing.T) {
	ctx := context.Background()
	for _, failure := range []string{"/" + indexDir, "/" + servingObject, "/" + releaseObject} {
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
				if strings.Contains(key, "/"+indexDir) {
					writes++
				}
			}
			if writes != shardCount(r) {
				t.Fatalf("retry reused an orphaned projection: index writes=%d", writes)
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
	shard := IndexShard(good.File.Path)
	for name, change := range map[string]func(*ServingIndex){
		"schema":     func(x *ServingIndex) { x.SchemaVersion++ },
		"owner":      func(x *ServingIndex) { x.Owner = "foreign" },
		"site":       func(x *ServingIndex) { x.Site = "foreign" },
		"release":    func(x *ServingIndex) { x.ReleaseID = strings.Repeat("0", 64) },
		"repository": func(x *ServingIndex) { x.RepositoryID = "foreign" },
		"created":    func(x *ServingIndex) { x.CreatedAt = x.CreatedAt.Add(time.Second) },
		"shard":      func(x *ServingIndex) { x.Shard = "zz" },
		"path": func(x *ServingIndex) {
			e := x.Files[good.File.Path]
			e.File.Path = "assets/other.js"
			x.Files[good.File.Path] = e
		},
		"digest": func(x *ServingIndex) {
			e := x.Files[good.File.Path]
			e.File.SHA256 = strings.Repeat("z", 64)
			x.Files[good.File.Path] = e
		},
		"html": func(x *ServingIndex) {
			e := x.Files[good.File.Path]
			e.File.ContentType = "text/html"
			x.Files[good.File.Path] = e
		},
		"misplaced": func(x *ServingIndex) { x.Files["elsewhere.js"] = x.Files[good.File.Path] },
	} {
		t.Run(name, func(t *testing.T) {
			bad := ServingIndex{SchemaVersion: good.SchemaVersion, Owner: good.Owner, Site: good.Site, ReleaseID: good.ReleaseID, RepositoryID: good.RepositoryID, CreatedAt: good.CreatedAt, Shard: shard, Files: map[string]ServingIndexEntry{good.File.Path: {File: good.File, Immutable: good.Immutable}}}
			change(&bad)
			if err := c.write(ctx, ServingIndexKey(header, shard), &bad); err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetServingFile(ctx, header, good.File.Path); !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted foreign or corrupt metadata: %v", err)
			}
		})
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
	store.failDelete = "/" + indexDir
	if err := c.DeleteRelease(ctx, s, r.ID); err == nil {
		t.Fatal("expected interrupted GC")
	}
	prefix := ReleasePrefix(r.Owner, r.Site, r.ID)
	if len(store.deletes) < 3 || store.deletes[0] != prefix+releaseObject || store.deletes[1] != prefix+servingObject {
		t.Fatalf("GC deleted data before withdrawing both completion objects: %v", store.deletes)
	}
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("interrupted GC still serves release: %v", err)
	}
	if _, err := store.Stat(ctx, prefix+uploadObject); err != nil {
		t.Fatalf("GC lost recovery marker: %v", err)
	}
	store.failDelete = ""
	if err := c.DeleteRelease(ctx, s, r.ID); err != nil {
		t.Fatal(err)
	}
	if store.deletes[len(store.deletes)-1] != prefix+uploadObject {
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
	if err := store.Delete(ctx, ReleasePrefix(r.Owner, r.Site, r.ID)+releaseObject); err != nil {
		t.Fatal(err)
	}
	if err := store.Delete(ctx, ServingIndexKey(header, IndexShard("assets/app.abc.js"))); err != nil {
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
		if strings.Contains(key, "/"+indexDir) {
			metadata++
		}
	}
	if metadata != shardCount(r) {
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

func TestServingProjectionRequiredWithoutManifestFallback(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("required-projection"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	header := servingRelease(r)
	if err := store.Delete(ctx, ServingReleaseKey(header)); err != nil {
		t.Fatal(err)
	}
	store.gets = nil
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("release without descriptor became readable: %v", err)
	}
	if len(store.gets) != 1 || store.gets[0] != ServingReleaseKey(header) {
		t.Fatalf("missing descriptor triggered an inventory or body read: %v", store.gets)
	}
}

func TestServingProjectionRejectsOtherFileMetadataVersions(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("metadata-version"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{0, fileMetadataVersion - 1, fileMetadataVersion + 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			header := servingRelease(r)
			header.FileMetadataVersion = version
			encoded, err := json.Marshal(header)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := DecodeServingRelease(encoded, r.Owner, r.Site, r.ID); !errors.Is(err, ErrInvalid) {
				t.Fatalf("cached unsupported metadata version accepted: %v", err)
			}
			if err := c.write(ctx, ServingReleaseKey(header), header); err != nil {
				t.Fatal(err)
			}
			if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsupported metadata version accepted: %v", err)
			}
			store.gets = nil
			if _, err := c.GetServingFile(ctx, header, "assets/app.abc.js"); !errors.Is(err, ErrInvalid) {
				t.Fatalf("unsupported file metadata version accepted: %v", err)
			}
			if len(store.gets) != 0 {
				t.Fatalf("invalid descriptor triggered a metadata read: %v", store.gets)
			}
		})
	}
}

func TestServingProjectionGCInterruptedBeforeDescriptorWithdrawal(t *testing.T) {
	ctx := context.Background()
	c, store := newProjectionCatalog()
	r, err := c.Publish(ctx, "foo", "source", origin("gc-descriptor"), projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	s := siteFor(r)
	s.Production = ""
	if err := c.SaveSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	store.failDelete = "/" + servingObject
	store.deletes = nil
	if err := c.DeleteRelease(ctx, s, r.ID); err == nil {
		t.Fatal("expected interrupted descriptor deletion")
	}
	prefix := ReleasePrefix(r.Owner, r.Site, r.ID)
	if len(store.deletes) != 2 || store.deletes[0] != prefix+releaseObject || store.deletes[1] != prefix+servingObject {
		t.Fatalf("GC did not withdraw manifest first: %v", store.deletes)
	}
	if _, err := c.GetRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("interrupted GC left a reusable complete release: %v", err)
	}
	if _, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("orphaned descriptor became readable: %v", err)
	}
	store.failDelete = ""
	r, err = c.Publish(ctx, r.Site, r.Owner, r.Origin, projectFiles())
	if err != nil {
		t.Fatal(err)
	}
	header, err := c.GetServingRelease(ctx, r.Owner, r.Site, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetServingFile(ctx, header, "assets/app.abc.js"); err != nil {
		t.Fatalf("reselected release did not restore current metadata: %v", err)
	}
}
