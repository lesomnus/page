package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

const (
	fileMetadataVersion       = 2
	maxServingReleaseMetadata = 128 << 10
	// A release holds at most a few tens of thousands of files spread over 256
	// shards; this bounds one shard far above that while keeping a lookup small.
	maxServingIndexMetadata = 4 << 20
)

// ServingRelease contains only release-scoped routing and identity. File
// inventories remain in the writer's manifest and are never retained here.
type ServingRelease struct {
	SchemaVersion       int            `json:"schemaVersion"`
	ID                  string         `json:"id"`
	Site                string         `json:"site"`
	Owner               string         `json:"owner"`
	Origin              Origin         `json:"origin"`
	CreatedAt           time.Time      `json:"createdAt"`
	Complete            bool           `json:"complete"`
	Pages               *routes.Config `json:"pages,omitempty"`
	FileMetadataVersion int            `json:"fileMetadataVersion"`
}

// ServingFile is one file record of a release. Its envelope prevents a record
// copied from another release or another path from authorizing a body lookup.
type ServingFile struct {
	SchemaVersion int       `json:"schemaVersion"`
	Owner         string    `json:"owner"`
	Site          string    `json:"site"`
	ReleaseID     string    `json:"releaseId"`
	RepositoryID  string    `json:"repositoryId"`
	CreatedAt     time.Time `json:"createdAt"`
	File          File      `json:"file"`
	Immutable     bool      `json:"immutable"`
}

func ServingReleaseKey(r *ServingRelease) string {
	return ReleasePrefix(r.Owner, r.Site, r.ID) + servingObject
}

// ServingIndex is one shard of a release's file records. Paths are sharded by
// the first byte of their SHA-256, so a lookup reads one small object and the
// release is written as at most 256 objects however many files it has. The
// envelope is checked per shard, as it was per file record.
type ServingIndex struct {
	SchemaVersion int                          `json:"schemaVersion"`
	Owner         string                       `json:"owner"`
	Site          string                       `json:"site"`
	ReleaseID     string                       `json:"releaseId"`
	RepositoryID  string                       `json:"repositoryId"`
	CreatedAt     time.Time                    `json:"createdAt"`
	Shard         string                       `json:"shard"`
	Files         map[string]ServingIndexEntry `json:"files"`
}

type ServingIndexEntry struct {
	File      File `json:"file"`
	Immutable bool `json:"immutable,omitempty"`
}

// IndexShard hashes the complete original path rather than creating a
// hierarchy from it. Thus a file named "a" cannot collide with "a/b" metadata.
func IndexShard(p string) string {
	h := sha256.Sum256([]byte(p))
	return hex.EncodeToString(h[:1])
}

func ServingIndexKey(r *ServingRelease, shard string) string {
	return ReleasePrefix(r.Owner, r.Site, r.ID) + indexDir + shard + ".json"
}

// ServingBlobKey is where the body of a file record of r is stored.
func ServingBlobKey(r *ServingRelease, f File) string { return BlobKey(r.Owner, r.Site, f.SHA256) }

func servingRelease(r *Release) *ServingRelease {
	return &ServingRelease{SchemaVersion: r.SchemaVersion, ID: r.ID, Site: r.Site, Owner: r.Owner, Origin: r.Origin, CreatedAt: r.CreatedAt, Complete: r.Complete, Pages: r.Pages, FileMetadataVersion: fileMetadataVersion}
}

func validateServingRelease(r *ServingRelease, owner, site, id string) error {
	// Reuse the immutable identity checks without constructing a file inventory.
	header := Release{SchemaVersion: r.SchemaVersion, ID: r.ID, Site: r.Site, Owner: r.Owner, Origin: r.Origin, CreatedAt: r.CreatedAt}
	if err := validateRelease(&header); err != nil {
		return err
	}
	if !r.Complete || r.Owner != owner || r.Site != site || r.ID != id || r.FileMetadataVersion != fileMetadataVersion {
		return fmt.Errorf("%w: serving release identity or schema", ErrInvalid)
	}
	return nil
}

func validateServingFile(r *ServingRelease, p string, f *ServingFile) error {
	if f.SchemaVersion != SchemaVersion || f.Owner != r.Owner || f.Site != r.Site || f.ReleaseID != r.ID || f.RepositoryID != r.Origin.RepositoryID || !f.CreatedAt.Equal(r.CreatedAt) {
		return fmt.Errorf("%w: serving file identity or schema", ErrInvalid)
	}
	if err := validateFile(p, f.File); err != nil {
		return err
	}
	if f.Immutable && !immutableFileAllowed(f.File) {
		return fmt.Errorf("%w: HTML or routing metadata cannot be an immutable fallback asset", ErrInvalid)
	}
	return nil
}

func validateServingIndex(r *ServingRelease, shard string, x *ServingIndex) error {
	if x.SchemaVersion != SchemaVersion || x.Owner != r.Owner || x.Site != r.Site || x.ReleaseID != r.ID || x.RepositoryID != r.Origin.RepositoryID || !x.CreatedAt.Equal(r.CreatedAt) || x.Shard != shard {
		return fmt.Errorf("%w: serving index identity or schema", ErrInvalid)
	}
	for p, e := range x.Files {
		if IndexShard(p) != shard {
			return fmt.Errorf("%w: serving index shard", ErrInvalid)
		}
		if err := validateServingFile(r, p, x.file(p, e)); err != nil {
			return err
		}
	}
	return nil
}

func (x *ServingIndex) file(p string, e ServingIndexEntry) *ServingFile {
	return &ServingFile{SchemaVersion: x.SchemaVersion, Owner: x.Owner, Site: x.Site, ReleaseID: x.ReleaseID, RepositoryID: x.RepositoryID, CreatedAt: x.CreatedAt, File: e.File, Immutable: e.Immutable}
}

// Lookup returns the record of p, or storage.ErrNotFound when the release has
// no such file.
func (x *ServingIndex) Lookup(p string) (*ServingFile, error) {
	e, ok := x.Files[p]
	if !ok {
		return nil, storage.ErrNotFound
	}
	return x.file(p, e), nil
}

func validateFile(p string, f File) error {
	if !validPath(p) || f.Path != p || f.Size < 0 || len(f.SHA256) != 64 || f.ContentType == "" {
		return fmt.Errorf("%w: release file %q", ErrInvalid, p)
	}
	if _, err := hex.DecodeString(f.SHA256); err != nil {
		return fmt.Errorf("%w: file digest", ErrInvalid)
	}
	return nil
}

func immutableFileAllowed(f File) bool {
	ext := strings.ToLower(path.Ext(f.Path))
	typ, _, _ := mime.ParseMediaType(f.ContentType)
	return ext != ".html" && ext != ".htm" && ext != ".xhtml" && !strings.EqualFold(path.Base(f.Path), "pages.json") && typ != "text/html" && typ != "application/xhtml+xml"
}

func (c *Catalog) readBounded(ctx context.Context, key string, limit int64, v any) error {
	b, err := c.readRaw(ctx, key, limit)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, key, err)
	}
	return nil
}

func (c *Catalog) readRaw(ctx context.Context, key string, limit int64) ([]byte, error) {
	body, info, err := c.Store.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer body.Close()
	if info.Size < 0 || info.Size > limit {
		return nil, fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	return b, nil
}

func (c *Catalog) writeBounded(ctx context.Context, key string, limit int, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > limit {
		return fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	if _, err := c.Store.Put(ctx, key, bytes.NewReader(b), int64(len(b)), "application/json"); err != nil {
		return err
	}
	c.notifyWrite(key)
	return nil
}

// GetServingRelease reads the small serving projection and checks the original
// completion boundary. An orphaned projection never makes an upload visible.
func (c *Catalog) GetServingRelease(ctx context.Context, owner, site, id string) (*ServingRelease, error) {
	if !ValidName(owner) || !ValidName(site) || !ValidID(id) {
		return nil, ErrInvalid
	}
	info, err := c.Store.Stat(ctx, ReleasePrefix(owner, site, id)+releaseObject)
	if err != nil {
		return nil, err
	}
	if info.Size < 0 || info.Size > maxMetadata {
		return nil, fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	r := &ServingRelease{}
	err = c.readBounded(ctx, ReleasePrefix(owner, site, id)+servingObject, maxServingReleaseMetadata, r)
	if err != nil {
		return nil, err
	}
	if err := validateServingRelease(r, owner, site, id); err != nil {
		return nil, err
	}
	return r, nil
}

// DecodeServingRelease validates a compact cached descriptor without loading
// its file inventory. Request routing performs its own structural validation.
func DecodeServingRelease(data []byte, owner, site, id string) (*ServingRelease, error) {
	if len(data) > maxServingReleaseMetadata {
		return nil, ErrInvalid
	}
	var release ServingRelease
	if err := json.Unmarshal(data, &release); err != nil {
		return nil, fmt.Errorf("%w: cached serving release: %v", ErrInvalid, err)
	}
	if err := validateServingRelease(&release, owner, site, id); err != nil {
		return nil, err
	}
	return &release, nil
}

// LoadServingIndex reads one index shard of r without loading the writer's
// inventory. A missing shard means the release has no file in it.
func (c *Catalog) LoadServingIndex(ctx context.Context, r *ServingRelease, shard string) ([]byte, error) {
	if r == nil || !ValidName(r.Owner) || !ValidName(r.Site) || !ValidID(r.ID) || len(shard) != 2 {
		return nil, ErrInvalid
	}
	if err := validateServingRelease(r, r.Owner, r.Site, r.ID); err != nil {
		return nil, err
	}
	return c.readRaw(ctx, ServingIndexKey(r, shard), maxServingIndexMetadata)
}

// DecodeServingIndex validates a shard read by LoadServingIndex or a cache.
func DecodeServingIndex(data []byte, r *ServingRelease, shard string) (*ServingIndex, error) {
	if len(data) > maxServingIndexMetadata {
		return nil, ErrInvalid
	}
	var x ServingIndex
	if err := json.Unmarshal(data, &x); err != nil {
		return nil, fmt.Errorf("%w: serving index: %v", ErrInvalid, err)
	}
	if err := validateServingIndex(r, shard, &x); err != nil {
		return nil, err
	}
	return &x, nil
}

// GetServingFile reads the record of one path without a cache. Serving reads
// the same shards through its metadata cache.
func (c *Catalog) GetServingFile(ctx context.Context, r *ServingRelease, p string) (*ServingFile, error) {
	if !validPath(p) {
		return nil, ErrInvalid
	}
	shard := IndexShard(p)
	data, err := c.LoadServingIndex(ctx, r, shard)
	if err != nil {
		return nil, err
	}
	x, err := DecodeServingIndex(data, r, shard)
	if err != nil {
		return nil, err
	}
	return x.Lookup(p)
}

func (c *Catalog) writeServingProjection(ctx context.Context, r *Release) error {
	e, err := releaseEngine(r)
	if err != nil {
		return err
	}
	header := servingRelease(r)
	shards := map[string]*ServingIndex{}
	for p, f := range r.Files {
		shard := IndexShard(p)
		x := shards[shard]
		if x == nil {
			x = &ServingIndex{SchemaVersion: SchemaVersion, Owner: r.Owner, Site: r.Site, ReleaseID: r.ID, RepositoryID: r.Origin.RepositoryID, CreatedAt: r.CreatedAt, Shard: shard, Files: map[string]ServingIndexEntry{}}
			shards[shard] = x
		}
		x.Files[p] = ServingIndexEntry{File: f, Immutable: e.Immutable(p)}
	}
	names := make([]string, 0, len(shards))
	for shard := range shards {
		names = append(names, shard)
	}
	sort.Strings(names)
	for _, shard := range names {
		x := shards[shard]
		if err := validateServingIndex(header, shard, x); err != nil {
			return err
		}
		if err := c.writeBounded(ctx, ServingIndexKey(header, shard), maxServingIndexMetadata, x); err != nil {
			return err
		}
	}
	return c.writeBounded(ctx, ServingReleaseKey(header), maxServingReleaseMetadata, header)
}
