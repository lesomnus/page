package deploy

import (
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
	if err := c.prepareSite(ctx, s, nil); err != nil {
		return err
	}
	return c.write(ctx, SiteKey(s.Name), s)
}

func (c *Catalog) GetRelease(ctx context.Context, owner, site, id string) (*Release, error) {
	if !ValidName(owner) || !ValidName(site) || !ValidID(id) {
		return nil, ErrInvalid
	}
	var r Release
	if err := c.read(ctx, ReleasePrefix(owner, site, id)+"manifest.json", &r); err != nil {
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

// Publish writes an owned upload marker, all file objects and serving metadata,
// and finally the immutable manifest. It does not move any public reference.
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
	if err := c.write(ctx, ReleasePrefix(owner, site, id)+"upload.json", r); err != nil {
		return nil, err
	}
	for _, f := range files {
		body, err := f.Open()
		if err != nil {
			return nil, err
		}
		h := sha256.New()
		count := &countWriter{w: h}
		typ := f.ContentType
		if typ == "" {
			typ = mime.TypeByExtension(path.Ext(f.Path))
		}
		if typ == "" {
			typ = "application/octet-stream"
		}
		_, putErr := c.Store.Put(ctx, FileKey(r, f.Path), io.TeeReader(io.LimitReader(body, f.Size+1), count), f.Size, typ)
		if putErr != nil {
			body.Close()
			return nil, putErr
		}
		var extra [1]byte
		n, readErr := body.Read(extra[:])
		closeErr := body.Close()
		if count.n != f.Size || n != 0 || readErr != io.EOF {
			return nil, fmt.Errorf("%w: file length changed: %s", ErrInvalid, f.Path)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		r.Files[f.Path] = File{Path: f.Path, Size: f.Size, ContentType: typ, SHA256: hex.EncodeToString(h.Sum(nil))}
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
	if err := c.write(ctx, ReleasePrefix(owner, site, id)+"manifest.json", r); err != nil {
		return nil, err
	}
	return r, nil
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
		if len(parts) == 2 && ValidID(parts[0]) && (parts[1] == "manifest.json" || parts[1] == "upload.json") {
			ids[parts[0]] = true
		}
	}
	var out []*Release
	for id := range ids {
		r, err := c.GetRelease(ctx, owner, site, id)
		if errorsIsNotFound(err) {
			var pending Release
			err = c.read(ctx, ReleasePrefix(owner, site, id)+"upload.json", &pending)
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
		err = c.read(ctx, ReleasePrefix(s.Owner, s.Name, id)+"upload.json", &marker)
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
			if key == prefix+"manifest.json" {
				return 0
			}
			if key == prefix+"serving.json" {
				return 1
			}
			if key == prefix+"upload.json" {
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

func errorsIsNotFound(err error) bool { return errors.Is(err, storage.ErrNotFound) }
