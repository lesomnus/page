package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/routes"
	"github.com/lesomnus/page/internal/storage"
)

func TestRoutesCommandsSharePublicationValidation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if e := os.WriteFile(filepath.Join(dir, name), []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("index.html", "home")
	write("about.html", "about")
	write("pages.json", `{"schemaVersion":1,"routing":{"cleanUrls":true,"fallback":{"rewrite":"/index.html"}}}`)
	var out, stderr bytes.Buffer
	if e := Run(context.Background(), []string{"routes", "validate", dir}, &out, &stderr); e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(out.String(), `"valid": true`) {
		t.Fatal(out.String())
	}
	for _, tc := range []struct {
		args         []string
		action, path string
		status       int
	}{
		{[]string{"routes", "test", dir, "/about"}, routes.ActionFile, "about.html", 200},
		{[]string{"routes", "test", dir, "/dashboard"}, routes.ActionFallback, "index.html", 200},
		{[]string{"routes", "test", "--navigation=false", dir, "/dashboard"}, routes.ActionNotFound, "", 404},
	} {
		out.Reset()
		if e := Run(context.Background(), tc.args, &out, &stderr); e != nil {
			t.Fatal(e)
		}
		var d routes.Decision
		if e := json.Unmarshal(out.Bytes(), &d); e != nil {
			t.Fatal(e)
		}
		if d.Action != tc.action || d.Path != tc.path || d.Status != tc.status {
			t.Fatalf("%v => %#v", tc.args, d)
		}
	}
	// Extensionless HTML must be rejected just like publication MIME validation.
	write("html-asset", "<!doctype html><html>private document</html>")
	write("pages.json", `{"schemaVersion":1,"assets":{"immutable":["/html-asset"]}}`)
	if e := Run(context.Background(), []string{"routes", "validate", dir}, &out, &stderr); !errors.Is(e, deploy.ErrInvalid) {
		t.Fatalf("HTML declared immutable: %v", e)
	}
	write("pages.json", `{"schemaVersion":1}`)
	if e := os.Symlink("index.html", filepath.Join(dir, "link")); e != nil {
		t.Fatal(e)
	}
	if _, _, e := localRoutes(dir); e == nil {
		t.Fatal("symlink accepted")
	}
}

func TestControlFlagsAndPendingWait(t *testing.T) {
	got, e := orderedFlags([]string{"foo", "release", "--config", "cfg.yaml", "--source=source", "--wait", "1s"})
	want := []string{"--config", "cfg.yaml", "--source=source", "--wait", "1s", "foo", "release"}
	if e != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("flags %v %v", got, e)
	}
	ctx := context.Background()
	cat := deploy.New(storage.NewMemory())
	publish := func(rev string) *deploy.Release {
		t.Helper()
		r, e := cat.Publish(ctx, "foo", "source", deploy.Origin{Kind: "git", RepositoryID: "42", Revision: rev}, []deploy.InputFile{{Path: "index.html", Size: int64(len(rev)), Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(rev)), nil }}})
		if e != nil {
			t.Fatal(e)
		}
		return r
	}
	one, two := publish("one"), publish("two")
	s := &deploy.Site{SchemaVersion: 1, Name: "foo", Owner: "source", RepositoryID: "42", Production: one.ID}
	if e = cat.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	var out bytes.Buffer
	if e = runControl(ctx, cat, "promote", []string{"foo", two.ID}, "source", 0, &out); e != nil {
		t.Fatal(e)
	}
	var state controlStatus
	if e = json.Unmarshal(out.Bytes(), &state); e != nil {
		t.Fatal(e)
	}
	if state.Applied || state.Production != one.ID || state.Desired.Target != two.ID {
		t.Fatalf("CLI falsely activated: %#v", state)
	}
	out.Reset()
	if e = runControl(ctx, cat, "promote", []string{"foo", two.ID}, "source", time.Millisecond, &out); e == nil || !strings.Contains(e.Error(), "control saved") {
		t.Fatalf("timeout did not describe pending intent: %v", e)
	}
	if e = cat.SaveSite(ctx, s); e != nil {
		t.Fatal(e)
	}
	state, e = getControlStatus(ctx, cat, s)
	if e != nil || !state.Applied || state.Production != two.ID {
		t.Fatalf("owner acknowledgement: %#v %v", state, e)
	}
	out.Reset()
	if e = runControl(ctx, cat, "releases", []string{"foo"}, "foreign", 0, &out); !errors.Is(e, deploy.ErrOwnership) {
		t.Fatal(e)
	}
	if e = runControl(ctx, cat, "promote", []string{"foo"}, "", 0, &out); e == nil {
		t.Fatal("accepted missing release")
	}
}
