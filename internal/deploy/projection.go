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
)

const (
	fileMetadataVersion       = 1
	maxServingReleaseMetadata = 128 << 10
	maxServingFileMetadata    = 16 << 10
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

// ServingFile is a point-readable file record. Its envelope prevents a record
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
	return ReleasePrefix(r.Owner, r.Site, r.ID) + "serving.json"
}

// ServingFileKey hashes the complete original path rather than creating a
// hierarchy from it. Thus a file named "a" cannot collide with "a/b" metadata.
func ServingFileKey(r *ServingRelease, p string) string {
	h := sha256.Sum256([]byte(p))
	return ReleasePrefix(r.Owner, r.Site, r.ID) + "metadata/" + hex.EncodeToString(h[:]) + ".json"
}

func ServingObjectKey(r *ServingRelease, p string) string {
	return ReleasePrefix(r.Owner, r.Site, r.ID) + "files/" + p
}

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
	body, info, err := c.Store.Get(ctx, key)
	if err != nil {
		return err
	}
	defer body.Close()
	if info.Size < 0 || info.Size > limit {
		return fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	b, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return err
	}
	if int64(len(b)) > limit {
		return fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%w: %s: %v", ErrInvalid, key, err)
	}
	return nil
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
	info, err := c.Store.Stat(ctx, ReleasePrefix(owner, site, id)+"manifest.json")
	if err != nil {
		return nil, err
	}
	if info.Size < 0 || info.Size > maxMetadata {
		return nil, fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	r := &ServingRelease{}
	err = c.readBounded(ctx, ReleasePrefix(owner, site, id)+"serving.json", maxServingReleaseMetadata, r)
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

// GetServingFile reads one required file record without loading the writer's
// inventory. Missing sidecar metadata means the file is not published.
func (c *Catalog) GetServingFile(ctx context.Context, r *ServingRelease, p string) (*ServingFile, error) {
	if r == nil || !ValidName(r.Owner) || !ValidName(r.Site) || !ValidID(r.ID) || !validPath(p) {
		return nil, ErrInvalid
	}
	if err := validateServingRelease(r, r.Owner, r.Site, r.ID); err != nil {
		return nil, err
	}
	var f ServingFile
	if err := c.readBounded(ctx, ServingFileKey(r, p), maxServingFileMetadata, &f); err != nil {
		return nil, err
	}
	if err := validateServingFile(r, p, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

func (c *Catalog) writeServingProjection(ctx context.Context, r *Release) error {
	e, err := releaseEngine(r)
	if err != nil {
		return err
	}
	header := servingRelease(r)
	paths := make([]string, 0, len(r.Files))
	for p := range r.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		f := ServingFile{SchemaVersion: SchemaVersion, Owner: r.Owner, Site: r.Site, ReleaseID: r.ID, RepositoryID: r.Origin.RepositoryID, CreatedAt: r.CreatedAt, File: r.Files[p], Immutable: e.Immutable(p)}
		if err := validateServingFile(header, p, &f); err != nil {
			return err
		}
		if err := c.writeBounded(ctx, ServingFileKey(header, p), maxServingFileMetadata, &f); err != nil {
			return err
		}
	}
	return c.writeBounded(ctx, ServingReleaseKey(header), maxServingReleaseMetadata, header)
}
