package app

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/lesomnus/page/internal/deploy"
	"github.com/lesomnus/page/internal/storage"
)

func TestStatusDescribesTagFirstSelectionAndFallback(t *testing.T) {
	ctx := context.Background()
	cat := deploy.New(storage.NewMemory())
	publish := func(revision string) *deploy.Release {
		t.Helper()
		r, err := cat.Publish(ctx, "app", "source", deploy.Origin{Kind: "git", RepositoryID: "42", Revision: revision}, []deploy.InputFile{{Path: "index.html", Size: int64(len(revision)), Open: func() (io.ReadCloser, error) {
			return io.NopCloser(strings.NewReader(revision)), nil
		}}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	production, branch, tag := publish("production"), publish("branch"), publish("tag")
	branchToken, tagToken := deploy.AliasToken("branch", "refs/heads/release"), deploy.AliasToken("tag", "refs/tags/release")
	s := &deploy.Site{SchemaVersion: deploy.SchemaVersion, Name: "app", Owner: "source", RepositoryID: "42", Production: production.ID, Aliases: map[string]deploy.Alias{
		branchToken: {Kind: "branch", Name: "refs/heads/release", Revision: "branch", Release: branch.ID},
		tagToken:    {Kind: "tag", Name: "refs/tags/release", Revision: "tag", Release: tag.ID},
	}}
	if err := cat.SaveSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	type preview struct{ Kind, Ref, Revision, URL string }
	type site struct {
		Previews        []preview
		PreviewProblems []deploy.PreviewProblem
	}
	status := func(mode string) site {
		t.Helper()
		var out bytes.Buffer
		if err := Status(ctx, Config{BaseDomain: "example.com", PreviewAliasMode: mode}, cat, &out); err != nil {
			t.Fatal(err)
		}
		var sites []site
		if err := json.Unmarshal(out.Bytes(), &sites); err != nil || len(sites) != 1 {
			t.Fatalf("decode: %s %v", &out, err)
		}
		return sites[0]
	}
	got := status("tag-first")
	if len(got.Previews) != 1 || got.Previews[0] != (preview{"tag", "refs/tags/release", "tag", "https://release--app.example.com"}) {
		t.Fatalf("selected preview: %#v", got.Previews)
	}
	if len(got.PreviewProblems) != 1 || got.PreviewProblems[0].Kind != "branch" || got.PreviewProblems[0].QualifiedURL != "https://"+deploy.Host("example.com", "flat", "app", branchToken) {
		t.Fatalf("hidden branch: %#v", got.PreviewProblems)
	}
	got = status("")
	if len(got.Previews) != 2 || len(got.PreviewProblems) != 0 {
		t.Fatalf("qualified default: %#v", got)
	}
	for _, p := range got.Previews {
		if p.URL != "https://"+deploy.Host("example.com", "flat", "app", deploy.AliasToken(p.Kind, p.Ref)) {
			t.Fatalf("changed qualified URL: %#v", p)
		}
	}
	delete(s.Aliases, tagToken)
	if err := cat.SaveSite(ctx, s); err != nil {
		t.Fatal(err)
	}
	got = status("tag-first")
	if len(got.Previews) != 1 || got.Previews[0] != (preview{"branch", "refs/heads/release", "branch", "https://release--app.example.com"}) || len(got.PreviewProblems) != 0 {
		t.Fatalf("branch fallback: %#v", got)
	}
}
