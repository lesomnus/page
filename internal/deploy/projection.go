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
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

const (
	fileMetadataVersion       = 1
	maxServingReleaseMetadata = 128 << 10
	maxServingFileMetadata    = 16 << 10
	maxLegacyPages            = 64 << 10
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
	if !r.Complete || r.Owner != owner || r.Site != site || r.ID != id || (r.FileMetadataVersion != 0 && r.FileMetadataVersion != fileMetadataVersion) {
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
// Old releases are streamed without materializing their Files map.
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
	if errorsIsNotFound(err) {
		r, _, err = c.readLegacyServing(ctx, owner, site, id, "")
	} else if err == nil && r.FileMetadataVersion != fileMetadataVersion {
		err = fmt.Errorf("%w: serving file metadata version", ErrInvalid)
	}
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

// GetServingFile does a point read for new releases. A missing sidecar is a
// missing file, not a reason to load the writer's inventory. Legacy releases
// stream just one file record out of their bounded manifest instead.
func (c *Catalog) GetServingFile(ctx context.Context, r *ServingRelease, p string) (*ServingFile, error) {
	if r == nil || !ValidName(r.Owner) || !ValidName(r.Site) || !ValidID(r.ID) || !validPath(p) {
		return nil, ErrInvalid
	}
	if err := validateServingRelease(r, r.Owner, r.Site, r.ID); err != nil {
		return nil, err
	}
	var f ServingFile
	switch r.FileMetadataVersion {
	case fileMetadataVersion:
		if err := c.readBounded(ctx, ServingFileKey(r, p), maxServingFileMetadata, &f); err != nil {
			return nil, err
		}
	case 0:
		header, file, err := c.readLegacyServing(ctx, r.Owner, r.Site, r.ID, p)
		if err != nil {
			return nil, err
		}
		if header.Origin != r.Origin || !header.CreatedAt.Equal(r.CreatedAt) || !reflect.DeepEqual(header.Pages, r.Pages) {
			return nil, fmt.Errorf("%w: legacy serving release changed", ErrInvalid)
		}
		if file == nil {
			return nil, storage.ErrNotFound
		}
		immutable, err := routes.MatchImmutable(r.Pages, p)
		if err != nil {
			return nil, fmt.Errorf("%w: pages.json: %v", ErrInvalid, err)
		}
		f = ServingFile{SchemaVersion: SchemaVersion, Owner: r.Owner, Site: r.Site, ReleaseID: r.ID, RepositoryID: r.Origin.RepositoryID, CreatedAt: r.CreatedAt, File: *file, Immutable: immutable && immutableFileAllowed(*file)}
	default:
		return nil, fmt.Errorf("%w: serving file metadata version", ErrInvalid)
	}
	if err := validateServingFile(r, p, &f); err != nil {
		return nil, err
	}
	return &f, nil
}

// EnsureServingProjection upgrades an already completed release in its owning
// source's writer pass. Readers use the bounded legacy fallback and never write.
// Like Publish, this requires a single active writer for the source/site.
func (c *Catalog) EnsureServingProjection(ctx context.Context, r *Release) error {
	if r == nil {
		return ErrInvalid
	}
	if err := validateRelease(r); err != nil {
		return err
	}
	if !r.Complete {
		return fmt.Errorf("%w: incomplete release", ErrInvalid)
	}
	if err := c.loadPages(ctx, r); err != nil {
		return err
	}
	return c.ensureServingProjection(ctx, r)
}

func (c *Catalog) ensureServingProjection(ctx context.Context, r *Release) error {
	expected := servingRelease(r)
	var existing ServingRelease
	if err := c.readBounded(ctx, ServingReleaseKey(expected), maxServingReleaseMetadata, &existing); errorsIsNotFound(err) {
		return c.writeServingProjection(ctx, r)
	} else if err != nil {
		return err
	}
	if err := validateServingRelease(&existing, r.Owner, r.Site, r.ID); err != nil {
		return err
	}
	if !reflect.DeepEqual(&existing, expected) {
		return fmt.Errorf("%w: serving projection disagrees with manifest", ErrInvalid)
	}
	return nil
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

// readLegacyServing decodes one file record at a time. The decoder's working
// buffer follows the largest individual value, not the number of release files.
// Only the requested file and the old pages.json record survive the scan.
func (c *Catalog) readLegacyServing(ctx context.Context, owner, site, id, requested string) (*ServingRelease, *File, error) {
	key := ReleasePrefix(owner, site, id) + "manifest.json"
	body, info, err := c.Store.Get(ctx, key)
	if err != nil {
		return nil, nil, err
	}
	defer body.Close()
	if info.Size < 0 || info.Size > maxMetadata {
		return nil, nil, fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	limited := &io.LimitedReader{R: body, N: maxMetadata + 1}
	dec := json.NewDecoder(limited)
	dec.UseNumber()
	r := &ServingRelease{}
	var found, pagesFile *File
	count := 0
	parse := func() error {
		token, err := dec.Token()
		if err != nil || token != json.Delim('{') {
			return fmt.Errorf("manifest must be an object")
		}
		seen := map[string]bool{}
		for dec.More() {
			token, err := dec.Token()
			if err != nil {
				return err
			}
			name, ok := token.(string)
			if !ok {
				return fmt.Errorf("invalid manifest field")
			}
			var target any
			switch name {
			case "schemaVersion":
				target = &r.SchemaVersion
			case "id":
				target = &r.ID
			case "site":
				target = &r.Site
			case "owner":
				target = &r.Owner
			case "origin":
				target = &r.Origin
			case "createdAt":
				target = &r.CreatedAt
			case "complete":
				target = &r.Complete
			case "pages":
				target = &r.Pages
			case "files":
				if seen[name] {
					return fmt.Errorf("duplicate manifest field %q", name)
				}
				seen[name] = true
				token, err := dec.Token()
				if err != nil || token != json.Delim('{') {
					return fmt.Errorf("files must be an object")
				}
				for dec.More() {
					token, err := dec.Token()
					if err != nil {
						return err
					}
					p, ok := token.(string)
					if !ok {
						return fmt.Errorf("invalid file path")
					}
					var f File
					if err := dec.Decode(&f); err != nil {
						return err
					}
					if err := validateFile(p, f); err != nil {
						return err
					}
					count++
					if p == requested {
						copy := f
						found = &copy
					}
					if p == "pages.json" {
						copy := f
						pagesFile = &copy
					}
				}
				if _, err := dec.Token(); err != nil {
					return err
				}
				continue
			default:
				if err := skipJSONValue(dec); err != nil {
					return err
				}
				continue
			}
			if seen[name] {
				return fmt.Errorf("duplicate manifest field %q", name)
			}
			seen[name] = true
			if err := dec.Decode(target); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
		if _, err := dec.Token(); err != io.EOF {
			return fmt.Errorf("trailing manifest data")
		}
		if limited.N == 0 {
			return fmt.Errorf("metadata too large")
		}
		return nil
	}
	if err := parse(); err != nil {
		return nil, nil, fmt.Errorf("%w: %s: %v", ErrInvalid, key, err)
	}
	if count == 0 {
		return nil, nil, fmt.Errorf("%w: empty release", ErrInvalid)
	}
	// Validate the envelope before using its identity to fetch legacy pages.json.
	if err := validateServingRelease(r, owner, site, id); err != nil {
		return nil, nil, err
	}
	if r.Pages == nil && pagesFile != nil {
		if err := c.loadLegacyServingPages(ctx, r, *pagesFile); err != nil {
			return nil, nil, err
		}
	}
	if err := validateServingRelease(r, owner, site, id); err != nil {
		return nil, nil, err
	}
	return r, found, nil
}

func (c *Catalog) loadLegacyServingPages(ctx context.Context, r *ServingRelease, f File) error {
	if f.Size > maxLegacyPages {
		return fmt.Errorf("%w: pages.json exceeds 64 KiB", ErrInvalid)
	}
	body, info, err := c.Store.Get(ctx, ServingObjectKey(r, "pages.json"))
	if err != nil {
		return err
	}
	defer body.Close()
	if info.Size < 0 || info.Size > maxLegacyPages {
		return fmt.Errorf("%w: pages.json exceeds 64 KiB", ErrInvalid)
	}
	b, err := io.ReadAll(io.LimitReader(body, maxLegacyPages+1))
	if err != nil {
		return err
	}
	h := sha256.Sum256(b)
	if int64(len(b)) != f.Size || hex.EncodeToString(h[:]) != f.SHA256 {
		return fmt.Errorf("%w: pages.json digest mismatch", ErrInvalid)
	}
	r.Pages, err = routes.Parse(b)
	if err != nil {
		return fmt.Errorf("%w: pages.json: %v", ErrInvalid, err)
	}
	return nil
}

// skipJSONValue consumes an unknown value without decoding nested inventories
// into interface maps or slices. json.Decoder enforces valid nesting itself.
func skipJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok || (delim != '{' && delim != '[') {
		return nil
	}
	depth := 1
	for depth > 0 {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		if delim, ok := token.(json.Delim); ok {
			switch delim {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
	}
	return nil
}
