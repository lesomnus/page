package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
)

func checkContract(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	for _, key := range []string{"files/a.txt", "files/b.txt", "files/c.txt"} {
		body := "content of " + key
		info, err := store.Put(ctx, key, strings.NewReader(body), int64(len(body)), "text/plain")
		if err != nil {
			t.Fatal(err)
		}
		if info.Key != key || info.Size != int64(len(body)) {
			t.Fatalf("put info: %+v", info)
		}
	}
	t.Cleanup(func() {
		for _, key := range []string{"files/a.txt", "files/b.txt", "files/c.txt"} {
			_ = store.Delete(ctx, key)
		}
	})
	r, info, err := store.Get(ctx, "files/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(old) != "content of files/a.txt" || info.ContentType != "text/plain" || info.ETag == "" {
		t.Fatalf("get: %q %+v %v", old, info, err)
	}
	stat, err := store.Stat(ctx, "files/a.txt")
	if err != nil || stat.Size != int64(len(old)) || stat.ETag != info.ETag {
		t.Fatalf("stat: %+v %v", stat, err)
	}
	r, info, err = store.GetRange(ctx, "files/a.txt", 2, 5)
	if err != nil {
		t.Fatal(err)
	}
	ranged, err := io.ReadAll(r)
	r.Close()
	if err != nil || !bytes.Equal(ranged, old[2:7]) || info.Size != 5 {
		t.Fatalf("range: %q %+v %v", ranged, info, err)
	}
	var keys []string
	cursor := ""
	for {
		page, err := store.List(ctx, "files/", cursor, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, obj := range page.Objects {
			keys = append(keys, obj.Key)
		}
		if page.NextCursor == "" {
			break
		}
		if page.NextCursor == cursor {
			t.Fatal("cursor did not advance")
		}
		cursor = page.NextCursor
	}
	if strings.Join(keys, ",") != "files/a.txt,files/b.txt,files/c.txt" {
		t.Fatalf("list: %v", keys)
	}
	if _, err = store.Put(ctx, "files/a.txt", strings.NewReader("new"), 3, "text/html"); err != nil {
		t.Fatal(err)
	}
	r, info, err = store.Get(ctx, "files/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	newBody, err := io.ReadAll(r)
	r.Close()
	if err != nil || string(newBody) != "new" || info.ContentType != "text/html" {
		t.Fatalf("replace: %q %+v %v", newBody, info, err)
	}
	if err = store.Delete(ctx, "files/a.txt"); err != nil {
		t.Fatal(err)
	}
	if err = store.Delete(ctx, "files/a.txt"); err != nil {
		t.Fatal("delete is not idempotent:", err)
	}
	if _, err = store.Stat(ctx, "files/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing stat: %v", err)
	}
	if _, _, err = store.Get(ctx, "files/a.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing get: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, _, err = store.Get(canceled, "files/b.txt"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	for _, key := range []string{"../secret", "/absolute", "a/../secret", "a//b", "a\\b", "a\nheader"} {
		if _, err = store.Put(ctx, key, strings.NewReader("x"), 1, "text/plain"); !errors.Is(err, ErrInvalidKey) {
			t.Fatalf("invalid key %q: %v", key, err)
		}
	}
}

func TestMemoryContract(t *testing.T) { checkContract(t, NewMemory()) }

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("upload interrupted") }

func TestMemoryFailedReplaceKeepsOldObject(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if _, err := m.Put(ctx, "meta.json", strings.NewReader("old"), 3, "application/json"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Put(ctx, "meta.json", brokenReader{}, 10, "application/json"); err == nil {
		t.Fatal("expected upload error")
	}
	if _, err := m.Put(ctx, "meta.json", strings.NewReader("short"), 10, "application/json"); err == nil {
		t.Fatal("expected size error")
	}
	r, _, err := m.Get(ctx, "meta.json")
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	b, _ := io.ReadAll(r)
	if string(b) != "old" {
		t.Fatalf("failed upload changed published object: %q", b)
	}
}

func TestMemoryReadersSeeWholeReplacements(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	a := strings.Repeat("a", 1024)
	b := strings.Repeat("b", 1024)
	if _, err := m.Put(ctx, "meta", strings.NewReader(a), 1024, ""); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 100 {
				r, _, err := m.Get(ctx, "meta")
				if err != nil {
					t.Error(err)
					return
				}
				data, err := io.ReadAll(r)
				r.Close()
				if err != nil || (string(data) != a && string(data) != b) {
					t.Error("partial read", err)
					return
				}
			}
		})
	}
	for i := range 100 {
		body := a
		if i%2 == 0 {
			body = b
		}
		if _, err := m.Put(ctx, "meta", strings.NewReader(body), 1024, ""); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestMemoryInvalidCursorAndRanges(t *testing.T) {
	m := NewMemory()
	ctx := context.Background()
	if _, err := m.List(ctx, "", "not+base64", 1); err == nil {
		t.Fatal("expected invalid cursor")
	}
	if _, err := m.List(ctx, "../", "", 1); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("prefix: %v", err)
	}
	for _, limit := range []int{-1, 0, 1001} {
		if _, err := m.List(ctx, "", "", limit); err == nil {
			t.Fatal("invalid limit", limit)
		}
	}
	_, _ = m.Put(ctx, "key", strings.NewReader("abc"), 3, "")
	for _, r := range [][2]int64{{-1, 1}, {0, 0}, {3, 1}} {
		if _, _, err := m.GetRange(ctx, "key", r[0], r[1]); err == nil {
			t.Fatal(fmt.Sprint("invalid range", r))
		}
	}
}
