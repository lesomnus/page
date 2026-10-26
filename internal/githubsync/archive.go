package githubsync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
	"gopkg.in/yaml.v3"
)

type manifest struct {
	SchemaVersion manifestVersion `yaml:"schemaVersion"`
	Publish       struct {
		Directory string `yaml:"directory"`
	} `yaml:"publish"`
}

// Keep the integer schema contract: YAML's usual scalar coercions would turn
// schemaVersion: 1.5 into 1 or accept the quoted string "1".
type manifestVersion int

func (v *manifestVersion) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag != "!!int" {
		return errors.New("schemaVersion must be an integer")
	}
	var parsed int
	if err := n.Decode(&parsed); err != nil {
		return err
	}
	*v = manifestVersion(parsed)
	return nil
}

type manifestDecodeError struct {
	filename string
	cause    error
}

// YAML type errors can quote scalar contents from a private repository. Keep
// that diagnostic available to callers without rendering it into sync logs.
func (e *manifestDecodeError) Error() string {
	return "decode " + e.filename + ": invalid deployment YAML"
}
func (e *manifestDecodeError) Unwrap() error { return e.cause }

func safePath(p string, root bool) bool {
	if root && p == "." {
		return true
	}
	if p == "" || strings.HasPrefix(p, "/") || !utf8.ValidString(p) || strings.ContainsAny(p, "\\\x00\r\n") || strings.ContainsAny(p, "\t") {
		return false
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return path.Clean(p) == p
}

func (s *Sync) deploymentManifest(ctx context.Context, repo repository, sha string) (manifest, error) {
	paths := []string{s.cfg.ManifestPath}
	if s.cfg.ManifestPath == "page.yaml" {
		// Immutable older commits still carry the previous deployment file.
		// A present but invalid page.yaml never falls back to another file.
		paths = append(paths, "manifest.json")
	}
	for _, filename := range paths {
		endpoint := repoEndpoint(repo.FullName) + "/contents/" + escapePath(filename) + "?ref=" + url.QueryEscape(sha)
		resp, err := s.request(ctx, endpoint, "application/vnd.github.raw+json")
		if err != nil {
			var ae *apiError
			if errors.As(err, &ae) && ae.status == 404 {
				continue
			}
			return manifest{}, err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
		resp.Body.Close()
		if err != nil {
			return manifest{}, err
		}
		if len(data) > 65536 {
			return manifest{}, errors.New("deployment manifest exceeds 64 KiB")
		}
		var m manifest
		decoder := yaml.NewDecoder(bytes.NewReader(data))
		decoder.KnownFields(true)
		if err := decoder.Decode(&m); err != nil {
			return manifest{}, &manifestDecodeError{filename, err}
		}
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			return manifest{}, fmt.Errorf("%s must contain one YAML document", filename)
		}
		if m.SchemaVersion != 1 || !safePath(m.Publish.Directory, true) {
			return manifest{}, errors.New("manifest needs schemaVersion 1 and a safe publish.directory")
		}
		return m, nil
	}
	return manifest{}, errNoManifest
}

func (s *Sync) publish(ctx context.Context, repo repository, site, sha string) (*deploy.Release, error) {
	m, err := s.deploymentManifest(ctx, repo, sha)
	if err != nil {
		return nil, err
	}
	origin := deploy.Origin{Kind: "git", RepositoryID: strconv.FormatInt(repo.ID, 10), Repository: repo.FullName, Revision: sha, Directory: m.Publish.Directory}
	id := deploy.ReleaseID(s.cfg.ID, site, origin)
	if r, err := s.cat.GetRelease(ctx, s.cfg.ID, site, id); err == nil {
		return r, nil
	} else if !errors.Is(err, storage.ErrNotFound) {
		return nil, err
	}
	files, cleanup, err := s.archive(ctx, repo.FullName, sha, m.Publish.Directory)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return s.cat.Publish(ctx, site, s.cfg.ID, origin, files)
}

func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func (s *Sync) archive(ctx context.Context, repo, sha, directory string) ([]deploy.InputFile, func(), error) {
	resp, err := s.request(ctx, repoEndpoint(repo)+"/tarball/"+sha, "application/vnd.github+json")
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	limited := &io.LimitedReader{R: resp.Body, N: s.cfg.MaxArchiveBytes + 1}
	gz, err := gzip.NewReader(limited)
	if err != nil {
		return nil, nil, err
	}
	defer gz.Close()
	dir, err := os.MkdirTemp("", "page-github-")
	if err != nil {
		return nil, nil, err
	}
	cleanup := func() { os.RemoveAll(dir) }
	failed := true
	defer func() {
		if failed {
			cleanup()
		}
	}()
	// Bound the complete expanded stream, including PAX metadata, directory
	// headers and padding that tar.Reader consumes without returning as files.
	expanded := &io.LimitedReader{R: gz, N: s.cfg.MaxArchiveBytes + 1}
	tr := tar.NewReader(expanded)
	files := []deploy.InputFile{}
	// Sparse tar members may produce logical bytes without reading those bytes
	// from gzip, so retain the aggregate logical-size limit as well.
	var total int64
	root := ""
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		h, err := tr.Next()
		if expanded.N <= 0 {
			return nil, nil, errors.New("expanded archive exceeds limit")
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if h.Size < 0 || total > s.cfg.MaxArchiveBytes-h.Size || total+h.Size > s.cfg.MaxArchiveBytes-512 {
			return nil, nil, errors.New("expanded archive exceeds limit")
		}
		total += h.Size + 512
		// GitHub's git archive begins with a global PAX header carrying the
		// commit ID. Its name is metadata, not a path in the repository tree.
		if h.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := strings.TrimSuffix(h.Name, "/")
		if !safePath(name, false) {
			return nil, nil, fmt.Errorf("unsafe archive path %q", h.Name)
		}
		top, rel, hasSlash := strings.Cut(name, "/")
		if root == "" {
			root = top
		}
		if root != top {
			return nil, nil, errors.New("archive has multiple roots")
		}
		if !hasSlash {
			if h.Typeflag != tar.TypeDir {
				return nil, nil, errors.New("archive has no repository root")
			}
			continue
		}
		file := rel
		if directory != "." {
			var ok bool
			file, ok = strings.CutPrefix(rel, directory+"/")
			if !ok {
				continue
			}
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			return nil, nil, fmt.Errorf("publish path %q is not a regular file", rel)
		}
		if !safePath(file, false) || h.Size > s.cfg.MaxFileBytes {
			return nil, nil, fmt.Errorf("invalid or oversized publish file %q", file)
		}
		if len(files) >= s.cfg.MaxFiles {
			return nil, nil, errors.New("too many publish files")
		}
		destination := filepath.Join(dir, filepath.FromSlash(file))
		if err := os.MkdirAll(filepath.Dir(destination), 0700); err != nil {
			return nil, nil, err
		}
		f, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return nil, nil, err
		}
		_, err = io.CopyN(f, tr, h.Size)
		closeErr := f.Close()
		if expanded.N <= 0 {
			return nil, nil, errors.New("expanded archive exceeds limit")
		}
		if err != nil {
			return nil, nil, err
		}
		if closeErr != nil {
			return nil, nil, closeErr
		}
		contentType := mime.TypeByExtension(path.Ext(file))
		if contentType == "" {
			f, err := os.Open(destination)
			if err != nil {
				return nil, nil, err
			}
			var sample [512]byte
			n, _ := f.Read(sample[:])
			f.Close()
			contentType = http.DetectContentType(sample[:n])
		}
		filename := destination
		files = append(files, deploy.InputFile{Path: file, Size: h.Size, ContentType: contentType, Open: func() (io.ReadCloser, error) { return os.Open(filename) }})
	}
	if limited.N <= 0 {
		return nil, nil, errors.New("compressed archive exceeds limit")
	}
	if len(files) == 0 {
		return nil, nil, errors.New("publish directory contains no regular files")
	}
	failed = false
	return files, cleanup, nil
}
