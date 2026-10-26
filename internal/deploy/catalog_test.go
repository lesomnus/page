package deploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

func input(p, body string) InputFile {
	return InputFile{Path: p, Size: int64(len(body)), Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }}
}
func origin(rev string) Origin {
	return Origin{Kind: "git", RepositoryID: "42", Repository: "team/foo.app", Revision: rev, Directory: "public"}
}
func siteFor(r *Release) *Site {
	return &Site{SchemaVersion: SchemaVersion, Name: r.Site, Owner: r.Owner, RepositoryID: r.Origin.RepositoryID, Repository: r.Origin.Repository, Production: r.ID, Aliases: map[string]Alias{}, Retired: map[string]time.Time{}}
}

func TestPublicationVisibilityOwnershipAndDeletion(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemory())
	r, err := c.Publish(ctx, "foo", "source-foo", origin("commit-1"), []InputFile{input("index.html", "old")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetSite(ctx, "foo"); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("unpublished site visible: %v", err)
	}
	s := siteFor(r)
	if err = c.SaveSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteRelease(ctx, s, r.ID); !errors.Is(err, ErrReferenced) {
		t.Fatalf("deleted live release: %v", err)
	}
	foreign := *s
	foreign.Owner = "another-source"
	if err = c.SaveSite(ctx, &foreign); !errors.Is(err, ErrOwnership) {
		t.Fatalf("ownership takeover: %v", err)
	}
	if _, err = c.Publish(ctx, "foo", "another-source", origin("commit-2"), []InputFile{input("index.html", "new")}); !errors.Is(err, ErrOwnership) {
		t.Fatalf("foreign publication: %v", err)
	}
	r2, err := c.Publish(ctx, "foo", "source-foo", origin("commit-2"), []InputFile{input("index.html", "new")})
	if err != nil {
		t.Fatal(err)
	}
	s.Production = r2.ID
	if err = c.SaveSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	if err = c.DeleteRelease(ctx, s, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = c.GetRelease(ctx, s.Owner, s.Name, r.ID); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("old release remains: %v", err)
	}
	if _, err = c.GetRelease(ctx, s.Owner, s.Name, r2.ID); err != nil {
		t.Fatal(err)
	}
}

type brokenStore struct {
	storage.Store
	fail bool
}

func (b *brokenStore) Put(ctx context.Context, key string, r io.Reader, n int64, typ string) (storage.Info, error) {
	if b.fail && strings.Contains(key, "/files/") {
		return storage.Info{}, errors.New("upload interrupted")
	}
	return b.Store.Put(ctx, key, r, n, typ)
}

func TestInterruptedUploadKeepsPreviousPublication(t *testing.T) {
	ctx := context.Background()
	s := &brokenStore{Store: storage.NewMemory()}
	c := New(s)
	r, err := c.Publish(ctx, "foo", "source-foo", origin("good"), []InputFile{input("index.html", "old")})
	if err != nil {
		t.Fatal(err)
	}
	published := siteFor(r)
	if err = c.SaveSite(ctx, published); err != nil {
		t.Fatal(err)
	}
	s.fail = true
	if _, err = c.Publish(ctx, "foo", "source-foo", origin("broken"), []InputFile{input("index.html", "new")}); err == nil {
		t.Fatal("failed upload succeeded")
	}
	id := ReleaseID("source-foo", "foo", origin("broken"))
	if _, err = c.GetRelease(ctx, "source-foo", "foo", id); !errors.Is(err, storage.ErrNotFound) {
		t.Fatalf("incomplete manifest published: %v", err)
	}
	current, err := c.GetSite(ctx, "foo")
	if err != nil || current.Production != r.ID {
		t.Fatalf("old production changed: %#v %v", current, err)
	}
	all, err := c.ListReleases(ctx, "source-foo", "foo")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("pending marker not recoverable: %d", len(all))
	}
	for _, x := range all {
		if x.ID == id && x.Complete {
			t.Fatal("pending release marked complete")
		}
	}
}

func TestInvalidMetadataAndInput(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemory())
	for _, p := range []string{"../x", "/x", "a/../x", "a//x", "a\\x"} {
		if _, e := c.Publish(ctx, "foo", "owner", origin("rev"), []InputFile{input(p, "data")}); !errors.Is(e, ErrInvalid) {
			t.Fatalf("accepted %q: %v", p, e)
		}
	}
	bad := input("index.html", "data")
	bad.Size = 3
	if _, e := c.Publish(ctx, "foo", "owner", origin("short"), []InputFile{bad}); e == nil {
		t.Fatal("accepted changed size")
	}
	if err := c.write(ctx, SiteKey("foo"), &Site{SchemaVersion: 2, Name: "foo", Owner: "owner", RepositoryID: "42"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetSite(ctx, "foo"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown schema accepted: %v", err)
	}
}
