package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory is an in-process object store for tests. It is not a production
// persistence backend. Objects are copied so readers observe a complete value.
type Memory struct {
	mu      sync.RWMutex
	objects map[string]memoryObject
}

type memoryObject struct {
	info Info
	body []byte
}

var _ Store = (*Memory)(nil)

func NewMemory() *Memory { return &Memory{objects: make(map[string]memoryObject)} }

func (m *Memory) Get(ctx context.Context, key string) (io.ReadCloser, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	if err := validKey(key); err != nil {
		return nil, Info{}, err
	}
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, Info{}, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(obj.body)), obj.info, nil
}

func (m *Memory) GetRange(ctx context.Context, key string, off, length int64) (io.ReadCloser, Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, Info{}, err
	}
	if err := validKey(key); err != nil {
		return nil, Info{}, err
	}
	if off < 0 || length <= 0 {
		return nil, Info{}, fmt.Errorf("invalid object range")
	}
	m.mu.RLock()
	obj, ok := m.objects[key]
	m.mu.RUnlock()
	if !ok {
		return nil, Info{}, ErrNotFound
	}
	info := obj.info
	if off >= info.Size {
		return nil, Info{}, fmt.Errorf("object range starts beyond end")
	}
	end := info.Size
	if length <= info.Size-off {
		end = off + length
	}
	info.Size = end - off
	return io.NopCloser(bytes.NewReader(obj.body[off:end])), info, nil
}

func (m *Memory) Stat(ctx context.Context, key string) (Info, error) {
	r, info, err := m.Get(ctx, key)
	if err == nil {
		r.Close()
	}
	return info, err
}

func (m *Memory) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) (Info, error) {
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if err := validKey(key); err != nil {
		return Info{}, err
	}
	if r == nil || size < 0 {
		return Info{}, fmt.Errorf("object body and nonnegative size are required")
	}
	body, err := io.ReadAll(&contextReader{ctx: ctx, r: r})
	if err != nil {
		return Info{}, err
	}
	if int64(len(body)) != size {
		return Info{}, fmt.Errorf("object size mismatch: got %d, want %d", len(body), size)
	}
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	info := Info{Key: key, Size: size, ContentType: contentType, ETag: fmt.Sprintf("\"%x\"", sha256.Sum256(body)), Modified: time.Now().UTC()}
	m.mu.Lock()
	m.objects[key] = memoryObject{info: info, body: body}
	m.mu.Unlock()
	return info, nil
}

func (m *Memory) List(ctx context.Context, prefix, cursor string, limit int) (Page, error) {
	if err := ctx.Err(); err != nil {
		return Page{}, err
	}
	if err := validPrefix(prefix); err != nil {
		return Page{}, err
	}
	if limit <= 0 || limit > 1000 {
		return Page{}, fmt.Errorf("list limit must be between 1 and 1000")
	}
	after := ""
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil {
			return Page{}, fmt.Errorf("invalid list cursor: %w", err)
		}
		after = string(decoded)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	keys := make([]string, 0)
	for key := range m.objects {
		if strings.HasPrefix(key, prefix) && key > after {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	page := Page{}
	count := min(len(keys), limit)
	for _, key := range keys[:count] {
		page.Objects = append(page.Objects, m.objects[key].info)
	}
	if len(keys) > count {
		page.NextCursor = base64.RawURLEncoding.EncodeToString([]byte(keys[count-1]))
	}
	return page, nil
}

func (m *Memory) Delete(ctx context.Context, key string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := validKey(key); err != nil {
		return err
	}
	m.mu.Lock()
	delete(m.objects, key)
	m.mu.Unlock()
	return nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
