package deploy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/lesomnus/page/internal/storage"
)

const maxMetadata = 16 << 20

// ValidFilePath reports whether p can name a file of a release.
func ValidFilePath(p string) bool { return validPath(p) }

func validPath(p string) bool {
	return p != "" && p != "." && p == path.Clean(p) && !strings.HasPrefix(p, "/") && !strings.ContainsAny(p, "\\\x00") && p != ".." && !strings.HasPrefix(p, "../")
}

func (c *Catalog) read(ctx context.Context, key string, v any) error {
	_, err := c.readInfo(ctx, key, v)
	return err
}

func (c *Catalog) readInfo(ctx context.Context, key string, v any) (storage.Info, error) {
	r, info, err := c.Store.Get(ctx, key)
	if err != nil {
		return storage.Info{}, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, maxMetadata+1))
	if err != nil {
		return storage.Info{}, err
	}
	if len(b) > maxMetadata {
		return storage.Info{}, fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	if err = json.Unmarshal(b, v); err != nil {
		return storage.Info{}, fmt.Errorf("%w: %s: %v", ErrInvalid, key, err)
	}
	return info, nil
}

func (c *Catalog) write(ctx context.Context, key string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(b) > maxMetadata {
		return fmt.Errorf("%w: metadata too large", ErrInvalid)
	}
	_, err = c.Store.Put(ctx, key, strings.NewReader(string(b)), int64(len(b)), "application/json")
	if err != nil {
		return err
	}
	c.notifyWrite(key)
	return nil
}

func validateSite(s *Site) error {
	if s.SchemaVersion != SchemaVersion || !ValidName(s.Name) || !ValidName(s.Owner) || s.RepositoryID == "" {
		return fmt.Errorf("%w: site identity or schema", ErrInvalid)
	}
	if s.Production != "" && !ValidID(s.Production) {
		return fmt.Errorf("%w: production release", ErrInvalid)
	}
	for token, a := range s.Aliases {
		if !ValidName(token) || !ValidID(a.Release) || a.Revision == "" || a.Name == "" || (a.Kind != "branch" && a.Kind != "tag" && a.Kind != "commit") {
			return fmt.Errorf("%w: preview alias", ErrInvalid)
		}
	}
	if s.AssetRetention < 0 {
		return ErrInvalid
	}
	for id, until := range s.AssetReleases {
		if !ValidID(id) || until.IsZero() {
			return ErrInvalid
		}
	}
	for _, id := range s.History {
		if !ValidID(id) {
			return ErrInvalid
		}
	}
	for id, t := range s.Retired {
		if !ValidID(id) || t.IsZero() {
			return fmt.Errorf("%w: retirement", ErrInvalid)
		}
	}
	return nil
}

func validateRelease(r *Release) error {
	if r.SchemaVersion != SchemaVersion || !ValidID(r.ID) || !ValidName(r.Site) || !ValidName(r.Owner) || r.Origin.Kind == "" || r.Origin.RepositoryID == "" || r.Origin.Revision == "" || r.CreatedAt.IsZero() || r.ID != ReleaseID(r.Owner, r.Site, r.Origin) {
		return fmt.Errorf("%w: release identity or schema", ErrInvalid)
	}
	for p, f := range r.Files {
		if err := validateFile(p, f); err != nil {
			return err
		}
	}
	if r.Complete && len(r.Files) == 0 {
		return fmt.Errorf("%w: empty release", ErrInvalid)
	}
	return nil
}

func (c *Catalog) GetSite(ctx context.Context, name string) (*Site, error) {
	s, _, err := c.GetSiteInfo(ctx, name)
	return s, err
}

func (c *Catalog) list(ctx context.Context, prefix string) ([]storage.Info, error) {
	var out []storage.Info
	cursor := ""
	seen := map[string]bool{}
	for {
		p, err := c.Store.List(ctx, prefix, cursor, 1000)
		if err != nil {
			return nil, err
		}
		out = append(out, p.Objects...)
		if p.NextCursor == "" {
			return out, nil
		}
		if p.NextCursor == cursor || seen[p.NextCursor] {
			return nil, fmt.Errorf("storage pagination did not advance")
		}
		seen[p.NextCursor] = true
		cursor = p.NextCursor
	}
}

func (c *Catalog) ListSites(ctx context.Context) ([]*Site, error) {
	objects, err := c.ListSiteObjects(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Site
	for _, o := range objects {
		name := strings.TrimSuffix(strings.TrimPrefix(o.Key, "sites/"), "/site.json")
		s, err := c.GetSite(ctx, name)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// SaveSite is the atomic visibility boundary. Only a single sync owns a site;
// simultaneous writers for the same owner are outside the MVP contract.
func (c *Catalog) SaveSite(ctx context.Context, s *Site) error {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	old, err := c.prepareSite(ctx, s, nil)
	if err != nil {
		return err
	}
	// A pass that changes nothing keeps the stored object. Rewriting it would
	// only move UpdatedAt, which re-keys serving caches and costs a storage
	// write. Local watchers are still told, as for a write: a save is also how
	// a source asks serving to retry a refresh that failed earlier.
	if same, err := sameSite(old, s); err != nil {
		return err
	} else if same {
		s.UpdatedAt = old.UpdatedAt
		c.notifyWrite(SiteKey(s.Name))
		return nil
	}
	return c.write(ctx, SiteKey(s.Name), s)
}

// sameSite reports whether next differs from prev only in UpdatedAt.
func sameSite(prev, next *Site) (bool, error) {
	if prev == nil {
		return false, nil
	}
	a := *prev
	b := *next
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	// A stored "aliases": {} decodes to an empty map; a derived site may hold nil.
	for _, s := range []*Site{&a, &b} {
		if s.Aliases == nil {
			s.Aliases = map[string]Alias{}
		}
	}
	x, err := json.Marshal(&a)
	if err != nil {
		return false, err
	}
	y, err := json.Marshal(&b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}

func (c *Catalog) GetRelease(ctx context.Context, owner, site, id string) (*Release, error) {
	if !ValidName(owner) || !ValidName(site) || !ValidID(id) {
		return nil, ErrInvalid
	}
	var r Release
	if err := c.read(ctx, ReleasePrefix(owner, site, id)+releaseObject, &r); err != nil {
		return nil, err
	}
	if err := validateRelease(&r); err != nil {
		return nil, err
	}
	if r.Owner != owner || r.Site != site || r.ID != id || !r.Complete {
		return nil, ErrInvalid
	}
	_, hasPages := r.Files["pages.json"]
	if hasPages != (r.Pages != nil) {
		return nil, fmt.Errorf("%w: persisted pages.json configuration missing or unexpected", ErrInvalid)
	}
	if _, err := releaseEngine(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// Publish writes an owned upload marker, the file bodies the site does not
// hold yet, serving metadata and finally the immutable manifest. It does not
// move any public reference.
func (c *Catalog) Publish(ctx context.Context, site, owner string, origin Origin, files []InputFile) (*Release, error) {
	if !ValidName(site) || !ValidName(owner) || origin.Kind == "" || origin.RepositoryID == "" || origin.Revision == "" || len(files) == 0 {
		return nil, ErrInvalid
	}
	if s, err := c.GetSite(ctx, site); err == nil {
		if s.Owner != owner || s.RepositoryID != origin.RepositoryID {
			return nil, ErrOwnership
		}
	} else if !errorsIsNotFound(err) {
		return nil, err
	}
	id := ReleaseID(owner, site, origin)
	if r, err := c.GetRelease(ctx, owner, site, id); err == nil {
		return r, nil
	} else if !errorsIsNotFound(err) {
		return nil, err
	}
	seen := map[string]bool{}
	for _, f := range files {
		if !validPath(f.Path) || f.Size < 0 || f.Size == math.MaxInt64 || f.Open == nil || seen[f.Path] {
			return nil, fmt.Errorf("%w: input file %q", ErrInvalid, f.Path)
		}
		seen[f.Path] = true
	}
	r := &Release{SchemaVersion: SchemaVersion, ID: id, Site: site, Owner: owner, Origin: origin, CreatedAt: time.Now().UTC(), Files: map[string]File{}}
	if err := c.write(ctx, ReleasePrefix(owner, site, id)+uploadObject, r); err != nil {
		return nil, err
	}
	for _, f := range files {
		typ := f.ContentType
		if typ == "" {
			typ = mime.TypeByExtension(path.Ext(f.Path))
		}
		if typ == "" {
			typ = "application/octet-stream"
		}
		digest, err := digestInput(f)
		if err != nil {
			return nil, err
		}
		if err := c.putBlob(ctx, owner, site, f, digest, typ); err != nil {
			return nil, err
		}
		r.Files[f.Path] = File{Path: f.Path, Size: f.Size, ContentType: typ, SHA256: digest}
	}
	if err := c.parsePagesForPublication(ctx, r); err != nil {
		return nil, err
	}
	r.Complete = true
	// Always rebuild this projection when the completion manifest was absent.
	// An interrupted upload or GC may have left older projection objects behind.
	if err := c.writeServingProjection(ctx, r); err != nil {
		return nil, err
	}
	if err := c.write(ctx, ReleasePrefix(owner, site, id)+releaseObject, r); err != nil {
		return nil, err
	}
	return r, nil
}

// digestInput hashes one input and checks that it has its declared length.
func digestInput(f InputFile) (string, error) {
	body, err := f.Open()
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(body, f.Size+1))
	closeErr := body.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n != f.Size {
		return "", fmt.Errorf("%w: file length changed: %s", ErrInvalid, f.Path)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// putBlob stores the body of f under its digest unless the site already holds
// it. The body is read again and must still match the digest; a body that
// changed in between is removed rather than left under a wrong address.
func (c *Catalog) putBlob(ctx context.Context, owner, site string, f InputFile, digest, typ string) error {
	key := BlobKey(owner, site, digest)
	if info, err := c.Store.Stat(ctx, key); err == nil && info.Size == f.Size {
		return nil
	} else if err != nil && !errorsIsNotFound(err) {
		return err
	}
	body, err := f.Open()
	if err != nil {
		return err
	}
	h := sha256.New()
	count := &countWriter{w: h}
	_, putErr := c.Store.Put(ctx, key, io.TeeReader(io.LimitReader(body, f.Size+1), count), f.Size, typ)
	if putErr != nil {
		body.Close()
		return putErr
	}
	var extra [1]byte
	n, readErr := body.Read(extra[:])
	closeErr := body.Close()
	if count.n != f.Size || n != 0 || readErr != io.EOF || hex.EncodeToString(h.Sum(nil)) != digest {
		if err := c.Store.Delete(ctx, key); err != nil && !errorsIsNotFound(err) {
			return err
		}
		return fmt.Errorf("%w: file changed during upload: %s", ErrInvalid, f.Path)
	}
	return closeErr
}

type countWriter struct {
	w io.Writer
	n int64
}

func (w *countWriter) Write(p []byte) (int, error) {
	n, e := w.w.Write(p)
	w.n += int64(n)
	return n, e
}

func (c *Catalog) ListReleases(ctx context.Context, owner, site string) ([]*Release, error) {
	if !ValidName(owner) || !ValidName(site) {
		return nil, ErrInvalid
	}
	prefix := "sources/" + owner + "/sites/" + site + "/releases/"
	objects, err := c.list(ctx, prefix)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	for _, o := range objects {
		parts := strings.Split(strings.TrimPrefix(o.Key, prefix), "/")
		if len(parts) == 2 && ValidID(parts[0]) && (parts[1] == releaseObject || parts[1] == uploadObject) {
			ids[parts[0]] = true
		}
	}
	var out []*Release
	for id := range ids {
		r, err := c.GetRelease(ctx, owner, site, id)
		if errorsIsNotFound(err) {
			var pending Release
			err = c.read(ctx, ReleasePrefix(owner, site, id)+uploadObject, &pending)
			if err == nil {
				err = validateRelease(&pending)
				if pending.Owner != owner || pending.Site != site || pending.ID != id || pending.Complete {
					err = ErrInvalid
				}
				r = &pending
			}
		}
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func referenced(s *Site, id string) bool {
	if until, ok := s.AssetReleases[id]; ok && until.After(time.Now().UTC()) {
		return true
	}
	if s.Production == id {
		return true
	}
	for _, a := range s.Aliases {
		if a.Release == id {
			return true
		}
	}
	return false
}

func (c *Catalog) DeleteRelease(ctx context.Context, s *Site, id string) error {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if !ValidName(s.Owner) || !ValidName(s.Name) || !ValidID(id) {
		return ErrInvalid
	}
	current, err := c.GetSite(ctx, s.Name)
	if errorsIsNotFound(err) {
		// A failed first upload has no published Site yet. Its durable marker
		// proves the same source owns this otherwise unreachable prefix.
		var marker Release
		err = c.read(ctx, ReleasePrefix(s.Owner, s.Name, id)+uploadObject, &marker)
		if errorsIsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = validateRelease(&marker); err != nil {
			return err
		}
		if marker.Owner != s.Owner || marker.Site != s.Name || marker.ID != id || marker.Origin.RepositoryID != s.RepositoryID {
			return ErrOwnership
		}
	} else if err != nil {
		return err
	} else if current.Owner != s.Owner || current.RepositoryID != s.RepositoryID {
		return ErrOwnership
	} else if referenced(current, id) {
		return ErrReferenced
	}
	ctrl, err := c.GetControl(ctx, s)
	if err != nil {
		return err
	}
	if (ctrl.Mode == "pinned" || ctrl.Mode == "paused") && ctrl.Target == id {
		return ErrReferenced
	}
	prefix := ReleasePrefix(s.Owner, s.Name, id)
	objects, err := c.list(ctx, prefix)
	if err != nil {
		return err
	}
	// Withdraw completeness before serving metadata or content. Otherwise an
	// interrupted GC could leave a complete release with a missing projection.
	// The upload marker is deleted last so interrupted deletion stays discoverable.
	sort.Slice(objects, func(i, j int) bool {
		priority := func(key string) int {
			if key == prefix+releaseObject {
				return 0
			}
			if key == prefix+servingObject {
				return 1
			}
			if key == prefix+uploadObject {
				return 3
			}
			return 2
		}
		pi, pj := priority(objects[i].Key), priority(objects[j].Key)
		if pi != pj {
			return pi < pj
		}
		return objects[i].Key < objects[j].Key
	})
	for _, o := range objects {
		if err := c.Store.Delete(ctx, o.Key); err != nil {
			return err
		}
	}
	return nil
}

// BlobGrace keeps recently written bodies out of collection. A source
// publishes and collects one site in sequence, so this only guards against an
// unexpected second writer, not against the source's own uploads.
const BlobGrace = time.Hour

// CollectBlobs deletes the bodies of s that no release of s lists, complete
// or pending. It returns how many it deleted.
func (c *Catalog) CollectBlobs(ctx context.Context, s *Site) (int, error) {
	c.mutation.Lock()
	defer c.mutation.Unlock()
	if !ValidName(s.Owner) || !ValidName(s.Name) {
		return 0, ErrInvalid
	}
	if current, err := c.GetSite(ctx, s.Name); err == nil {
		if current.Owner != s.Owner || current.RepositoryID != s.RepositoryID {
			return 0, ErrOwnership
		}
	} else if !errorsIsNotFound(err) {
		return 0, err
	}
	releases, err := c.ListReleases(ctx, s.Owner, s.Name)
	if err != nil {
		return 0, err
	}
	keep := map[string]bool{}
	for _, r := range releases {
		for _, f := range r.Files {
			keep[f.SHA256] = true
		}
	}
	prefix := blobPrefix(s.Owner, s.Name)
	objects, err := c.list(ctx, prefix)
	if err != nil {
		return 0, err
	}
	now := c.now()
	deleted := 0
	for _, o := range objects {
		digest := strings.TrimPrefix(o.Key, prefix)
		if !ValidID(digest) || keep[digest] || now.Sub(o.Modified) < BlobGrace {
			continue
		}
		if err := c.Store.Delete(ctx, o.Key); err != nil && !errorsIsNotFound(err) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

func errorsIsNotFound(err error) bool { return errors.Is(err, storage.ErrNotFound) }
