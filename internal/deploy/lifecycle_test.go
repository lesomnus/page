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

func publishPages(t *testing.T, c *Catalog, rev, config string, files ...InputFile) *Release {
	t.Helper()
	if config != "" {
		files = append(files, input("pages.json", config))
	}
	r, e := c.Publish(context.Background(), "foo", "source", origin(rev), files)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestRoutingValidationBeforeCompletionAndLegacyRead(t *testing.T) {
	ctx := context.Background()
	c := New(storage.NewMemory())
	old := publishPages(t, c, "old", "", input("index.html", "old"))
	if e := c.SaveSite(ctx, siteFor(old)); e != nil {
		t.Fatal(e)
	}
	_, e := c.Publish(ctx, "foo", "source", origin("bad"), []InputFile{input("index.html", "new"), input("pages.json", `{"schemaVersion":1,"routing":{"routes":[{"from":"/x","rewrite":"/missing.html"}]}}`)})
	if !errors.Is(e, ErrInvalid) {
		t.Fatalf("invalid routing accepted: %v", e)
	}
	if _, e = c.GetRelease(ctx, "source", "foo", ReleaseID("source", "foo", origin("bad"))); !errors.Is(e, storage.ErrNotFound) {
		t.Fatalf("bad release complete: %v", e)
	}
	s, e := c.GetSite(ctx, "foo")
	if e != nil || s.Production != old.ID {
		t.Fatalf("publication changed: %v %v", s, e)
	}
	valid := publishPages(t, c, "valid", `{"schemaVersion":1,"routing":{"cleanUrls":true}}`, input("index.html", "ok"))
	valid.Pages = nil // A manifest written before pages.json had server semantics.
	if e = c.write(ctx, ReleasePrefix(valid.Owner, valid.Site, valid.ID)+"manifest.json", valid); e != nil {
		t.Fatal(e)
	}
	read, e := c.GetRelease(ctx, "source", "foo", valid.ID)
	if e != nil || read.Pages == nil || !read.Pages.Routing.CleanURLs {
		t.Fatalf("legacy config not loaded: %v %v", read, e)
	}
	if _, e = c.Store.Put(ctx, FileKey(valid, "pages.json"), strings.NewReader("tampered"), 8, "application/json"); e != nil {
		t.Fatal(e)
	}
	if _, e = c.GetRelease(ctx, "source", "foo", valid.ID); !errors.Is(e, ErrInvalid) {
		t.Fatalf("legacy digest unchecked: %v", e)
	}
}

func TestImmutableURLCannotChangeThroughRouting(t *testing.T) {
	for _, tc := range []struct {
		name, config, file, content string
		reject                      bool
	}{
		{"same", `{"schemaVersion":1}`, "assets/app.hash.js", "same", false},
		{"changed", `{"schemaVersion":1}`, "assets/app.hash.js", "different", true},
		{"rewrite", `{"schemaVersion":1,"routing":{"routes":[{"from":"/assets/app.hash.js","rewrite":"/other.js"}]}}`, "other.js", "different", true},
		{"missing-rewrite", `{"schemaVersion":1,"routing":{"routes":[{"from":"/assets/*","rewrite":"/bundles/*"}]}}`, "bundles/new.js", "same", true},
		{"changed-type", `{"schemaVersion":1,"routing":{"routes":[{"from":"/assets/app.hash.js","rewrite":"/other.html"}]}}`, "other.html", "same", true},
		{"redirect", `{"schemaVersion":1,"routing":{"routes":[{"from":"/assets/app.hash.js","redirect":"/other.js"}]}}`, "other.js", "same", true},
		{"removed", `{"schemaVersion":1}`, "assets/new.hash.js", "new", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			c := New(storage.NewMemory())
			old := publishPages(t, c, "old", `{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`, input("index.html", "old"), input("assets/app.hash.js", "same"))
			s := siteFor(old)
			s.AssetRetention = time.Hour
			if e := c.SaveSite(ctx, s); e != nil {
				t.Fatal(e)
			}
			next := publishPages(t, c, "next", tc.config, input("index.html", "next"), input(tc.file, tc.content))
			s.Production = next.ID
			e := c.SaveSite(ctx, s)
			if tc.reject {
				if !errors.Is(e, ErrInvalid) {
					t.Fatalf("collision accepted: %v", e)
				}
				current, _ := c.GetSite(ctx, "foo")
				if current.Production != old.ID {
					t.Fatal("failed transition visible")
				}
			} else {
				if e != nil {
					t.Fatal(e)
				}
				if !s.AssetReleases[old.ID].After(time.Now()) {
					t.Fatal("asset lease missing")
				}
				if e := c.DeleteRelease(ctx, s, old.ID); !errors.Is(e, ErrReferenced) {
					t.Fatalf("asset lease ignored by GC: %v", e)
				}
			}
		})
	}
}

type failMetadataPut struct {
	storage.Store
	key string
}

func (s *failMetadataPut) Put(ctx context.Context, key string, body io.Reader, n int64, typ string) (storage.Info, error) {
	if key == s.key {
		return storage.Info{}, errors.New("injected metadata PUT failure")
	}
	return s.Store.Put(ctx, key, body, n, typ)
}

func TestControlIntentRecoveryAndRollback(t *testing.T) {
	ctx := context.Background()
	store := &failMetadataPut{Store: storage.NewMemory()}
	c := New(store)
	one := publishPages(t, c, "one", "", input("index.html", "one"))
	s := siteFor(one)
	if e := c.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	two := publishPages(t, c, "two", "", input("index.html", "two"))
	s.Production = two.ID
	if e := c.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	if _, e := c.RequestControl(ctx, "foo", "foreign", "pinned", one.ID); !errors.Is(e, ErrOwnership) {
		t.Fatal(e)
	}
	if _, e := c.RequestControl(ctx, "foo", "source", "pinned", strings.Repeat("0", 64)); !errors.Is(e, storage.ErrNotFound) {
		t.Fatal(e)
	}
	store.key = ControlKey("foo")
	if _, e := c.RequestRollback(ctx, "foo", "source", ""); e == nil {
		t.Fatal("failed control PUT accepted")
	}
	store.key = ""
	s, e := c.RequestRollback(ctx, "foo", "source", "")
	if e != nil || s.Production != two.ID {
		t.Fatalf("intent materialized early: %v %v", s, e)
	}
	ctrl, e := c.GetControl(ctx, s)
	if e != nil || ctrl.Target != one.ID || ctrl.Mode != "pinned" || s.ControlAppliedAt.Equal(ctrl.UpdatedAt) {
		t.Fatalf("rollback intent: %v %v", ctrl, e)
	}
	if e = c.DeleteRelease(ctx, s, one.ID); !errors.Is(e, ErrReferenced) {
		t.Fatalf("pending pin unprotected: %v", e)
	}
	if e = c.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	if s.Production != one.ID || !s.ControlAppliedAt.Equal(ctrl.UpdatedAt) {
		t.Fatal("source failed to acknowledge intent")
	}
	store.key = SiteKey("foo")
	if _, e = c.SetControl(ctx, "foo", "source", "pinned", two.ID); e == nil {
		t.Fatal("failed publication PUT accepted")
	}
	store.key = ""
	s, e = c.GetSite(ctx, "foo")
	if e != nil || s.Production != one.ID {
		t.Fatalf("partial publication: %v %v", s, e)
	}
	if e = c.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	if s.Production != two.ID {
		t.Fatal("durable intent not recovered")
	}
}
