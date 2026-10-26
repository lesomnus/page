package githubsync

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

// git archive/GitHub codeload exposes a global PAX comment before the root
// directory. tar.Reader returns that metadata header to the caller.
func githubArchive(t *testing.T, headers ...*tar.Header) []byte {
	t.Helper()
	var b bytes.Buffer
	gz := gzip.NewWriter(&b)
	tw := tar.NewWriter(gz)
	for _, h := range headers {
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := io.CopyN(tw, strings.NewReader("hello"), h.Size); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func archiveSync(t *testing.T, archive []byte, limit int64) *Sync {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/foo.app/tarball/"+sha(2) {
			t.Errorf("unexpected archive request: %s", r.URL.Path)
		}
		_, _ = w.Write(archive)
	}))
	t.Cleanup(server.Close)
	cfg := config(server.URL)
	if limit > 0 {
		cfg.MaxArchiveBytes = limit
	}
	return newSync(t, deploy.New(storage.NewMemory()), cfg)
}

func TestGitHubGlobalPAXHeaderPrecedesRepositoryRoot(t *testing.T) {
	root := "Holiday-Robot-test.hday.page-88bf964"
	archive := githubArchive(t,
		&tar.Header{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": sha(2)}},
		&tar.Header{Name: root + "/", Typeflag: tar.TypeDir, Mode: 0755},
		&tar.Header{Name: root + "/public/", Typeflag: tar.TypeDir, Mode: 0755},
		&tar.Header{Name: root + "/public/index.html", Typeflag: tar.TypeReg, Mode: 0644, Size: 5},
	)
	s := archiveSync(t, archive, 0)
	files, cleanup, err := s.archive(context.Background(), "acme/foo.app", sha(2), "public")
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if len(files) != 1 || files[0].Path != "index.html" {
		t.Fatalf("unexpected publish files: %+v", files)
	}
	body, err := files[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	if err != nil || string(data) != "hello" {
		t.Fatalf("wrong file body: %q %v", data, err)
	}
}

func TestGlobalPAXMetadataDoesNotRelaxArchiveTreeChecks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
	}{
		{"multiple-roots", []*tar.Header{{Name: "repo/", Typeflag: tar.TypeDir}, {Name: "other/public/index.html", Typeflag: tar.TypeReg, Size: 5}}},
		{"traversal", []*tar.Header{{Name: "repo/", Typeflag: tar.TypeDir}, {Name: "repo/public/../escape", Typeflag: tar.TypeReg, Size: 5}}},
		{"root-file", []*tar.Header{{Name: "index.html", Typeflag: tar.TypeReg, Size: 5}}},
		{"root-only", []*tar.Header{{Name: "repo/", Typeflag: tar.TypeDir}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := append([]*tar.Header{{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": sha(2)}}}, tc.headers...)
			s := archiveSync(t, githubArchive(t, headers...), 0)
			if _, cleanup, err := s.archive(context.Background(), "acme/foo.app", sha(2), "public"); err == nil {
				cleanup()
				t.Fatal("invalid archive tree was accepted")
			}
		})
	}
}

func TestExpandedArchiveLimitCountsMetadataDirectoriesAndPadding(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers []*tar.Header
		limit   int64
	}{
		{"global-pax-payload", []*tar.Header{{Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader, PAXRecords: map[string]string{"comment": strings.Repeat("x", 5000)}}}, 4096},
		{"root-directories", []*tar.Header{{Name: "repo/", Typeflag: tar.TypeDir}, {Name: "repo/", Typeflag: tar.TypeDir}, {Name: "repo/", Typeflag: tar.TypeDir}}, 1024},
		{"local-pax-payload", []*tar.Header{{Name: "repo/public/index.html", Typeflag: tar.TypeReg, Size: 5, Format: tar.FormatPAX, PAXRecords: map[string]string{"comment": strings.Repeat("x", 5000)}}}, 4096},
		{"file-padding", []*tar.Header{{Name: "repo/public/index.html", Typeflag: tar.TypeReg, Size: 5}}, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := archiveSync(t, githubArchive(t, tc.headers...), tc.limit)
			if _, cleanup, err := s.archive(context.Background(), "acme/foo.app", sha(2), "public"); err == nil {
				cleanup()
				t.Fatal("expanded archive limit was ignored")
			} else if !strings.Contains(err.Error(), "expanded archive exceeds limit") {
				t.Fatalf("wrong limit error: %v", err)
			}
		})
	}
}
