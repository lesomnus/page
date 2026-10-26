package deploy

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

type interruptedDelete struct {
	storage.Store
	deletes int
	fail    bool
}

func (s *interruptedDelete) Delete(ctx context.Context, key string) error {
	if s.fail && strings.Contains(key, "/files/") {
		s.deletes++
		if s.deletes == 2 {
			return errors.New("delete interrupted")
		}
	}
	return s.Store.Delete(ctx, key)
}

func TestPartialGCThenReselectionRepublishesCompleteFiles(t *testing.T) {
	ctx := context.Background()
	store := &interruptedDelete{Store: storage.NewMemory()}
	cat := New(store)
	files := []InputFile{input("a.html", "a"), input("b.html", "b")}
	r, e := cat.Publish(ctx, "foo", "source", origin("old"), files)
	if e != nil {
		t.Fatal(e)
	}
	s := siteFor(r)
	if e = cat.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	s.Production = ""
	if e = cat.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	store.fail = true
	if e = cat.DeleteRelease(ctx, s, r.ID); e == nil {
		t.Fatal("expected interrupted deletion")
	}
	if _, e = cat.GetRelease(ctx, "source", "foo", r.ID); !errors.Is(e, storage.ErrNotFound) {
		t.Fatalf("partial release still complete: %v", e)
	}
	all, e := cat.ListReleases(ctx, "source", "foo")
	if e != nil || len(all) != 1 || all[0].Complete {
		t.Fatalf("pending recovery marker lost: %v %v", all, e)
	}
	store.fail = false
	again, e := cat.Publish(ctx, "foo", "source", origin("old"), files)
	if e != nil {
		t.Fatal(e)
	}
	for name := range again.Files {
		body, _, e := store.Get(ctx, FileKey(again, name))
		if e != nil {
			t.Fatal(e)
		}
		b, _ := io.ReadAll(body)
		body.Close()
		if len(b) != 1 {
			t.Fatal("file not restored")
		}
	}
}

func TestConcurrentClaimsAndCreationTime(t *testing.T) {
	ctx := context.Background()
	cat := New(storage.NewMemory())
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, owner := range []string{"one", "two"} {
		wg.Go(func() {
			<-start
			results <- cat.SaveSite(ctx, &Site{SchemaVersion: 1, Name: "foo", Owner: owner, RepositoryID: owner})
		})
	}
	close(start)
	wg.Wait()
	close(results)
	wins := 0
	for e := range results {
		if e == nil {
			wins++
		} else if !errors.Is(e, ErrOwnership) {
			t.Fatal(e)
		}
	}
	if wins != 1 {
		t.Fatalf("claims succeeded: %d", wins)
	}
	s, e := cat.GetSite(ctx, "foo")
	if e != nil {
		t.Fatal(e)
	}
	created := s.CreatedAt
	s.CreatedAt = time.Now().Add(time.Hour)
	if e = cat.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	if !s.CreatedAt.Equal(created) {
		t.Fatal("creation timestamp moved")
	}
}
