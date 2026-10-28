package githubsync

import (
	"context"
	"strings"
	"testing"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func TestSyncBackfillsExistingReleaseServingMetadataWithoutArchive(t *testing.T) {
	ctx := context.Background()
	forge, api := fixture(t)
	store := storage.NewMemory()
	cat := deploy.New(store)
	syncer := newSync(t, cat, config(api.URL))
	mustOnce(t, syncer)
	before := getSite(t, cat)
	releases, err := cat.ListReleases(ctx, before.Owner, before.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(releases) == 0 {
		t.Fatal("fixture has no releases")
	}
	// Model snapshots written before serving projections existed.
	for _, release := range releases {
		prefix := deploy.ReleasePrefix(release.Owner, release.Site, release.ID)
		objects, err := store.List(ctx, prefix, "", 1000)
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range objects.Objects {
			if object.Key == prefix+"serving.json" || strings.HasPrefix(object.Key, prefix+"metadata/") {
				if err := store.Delete(ctx, object.Key); err != nil {
					t.Fatal(err)
				}
			}
		}
		descriptor, err := cat.GetServingRelease(ctx, release.Owner, release.Site, release.ID)
		if err != nil || descriptor.FileMetadataVersion != 0 {
			t.Fatalf("legacy fixture not readable: %v", err)
		}
	}
	forge.mu.Lock()
	archives := forge.archiveReads
	forge.mu.Unlock()
	mustOnce(t, syncer)
	after := getSite(t, cat)
	if after.Production != before.Production {
		t.Fatal("migration changed production")
	}
	forge.mu.Lock()
	gotArchives := forge.archiveReads
	forge.mu.Unlock()
	if gotArchives != archives {
		t.Fatalf("migration downloaded archives again: %d -> %d", archives, gotArchives)
	}
	for _, release := range releases {
		descriptor, err := cat.GetServingRelease(ctx, release.Owner, release.Site, release.ID)
		if err != nil || descriptor.FileMetadataVersion != 1 {
			t.Fatalf("existing release not migrated: %v", err)
		}
		file, err := cat.GetServingFile(ctx, descriptor, "index.html")
		if err != nil || file.File.SHA256 == "" {
			t.Fatalf("missing backfilled file metadata: %v", err)
		}
	}
}
