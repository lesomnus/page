package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func configFile(t *testing.T, body string) string {
	t.Helper()
	name := filepath.Join(t.TempDir(), "page.yaml")
	if e := os.WriteFile(name, []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
	return name
}
func TestConfigurationAndRedaction(t *testing.T) {
	t.Setenv("GITHUB_TOKEN", "never-print-this")
	file := configFile(t, "base_domain: example.com\nrefresh_interval: 2s\ngithub:\n  - id: source\n    owner: org\n")
	c, e := Load(file)
	if e != nil || c.RefreshInterval != 2*time.Second || c.GitHub[0].Token != "never-print-this" {
		t.Fatalf("load: %v %v", c, e)
	}
	var out, errOut bytes.Buffer
	if e = Run(context.Background(), []string{"--config", file, "config"}, &out, &errOut); e != nil {
		t.Fatal(e)
	}
	if strings.Contains(out.String(), "never-print-this") || !strings.Contains(out.String(), "<redacted>") {
		t.Fatal("token was not redacted")
	}
	for _, bad := range []string{"typo: true\n", "base_domain: example.com\n---\nlisten: ':99'\n", "github:\n - id: same\n - id: same\n"} {
		if _, e = Load(configFile(t, bad)); e == nil {
			t.Fatalf("accepted config: %q", bad)
		}
	}
	if e = Run(context.Background(), []string{"serve", "--once", "--config", file}, &out, &errOut); e == nil {
		t.Fatal("accepted once on serve")
	}
}

func TestServingStopsWithoutSourceOrDatabase(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, Config{Listen: "127.0.0.1:0", APIListen: "127.0.0.1:0", BaseDomain: "example.com", RefreshInterval: time.Millisecond}, deploy.New(storage.NewMemory()))
	}()
	time.AfterFunc(30*time.Millisecond, cancel)
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("servers did not shut down")
	}
}

func TestShutdownDrainsActiveDownload(t *testing.T) {
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(started); <-release; io.WriteString(w, "complete") })}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- serveHTTP(ctx, server, l) }()
	response := make(chan string, 1)
	go func() {
		r, e := http.Get("http://" + l.Addr().String())
		if e != nil {
			response <- e.Error()
			return
		}
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		response <- string(b)
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("request did not start")
	}
	cancel()
	select {
	case <-stopped:
		t.Fatal("returned before download drained")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case body := <-response:
		if body != "complete" {
			t.Fatal(body)
		}
	case <-time.After(time.Second):
		t.Fatal("download lost")
	}
	select {
	case e := <-stopped:
		if !errors.Is(e, context.Canceled) {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("server did not stop")
	}
}
