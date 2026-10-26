package deploy

import (
	"reflect"
	"strings"
	"testing"
)

func previewSite(name string, aliases ...Alias) *Site {
	s := &Site{Name: name, Production: strings.Repeat("a", 64), Aliases: map[string]Alias{}}
	for _, a := range aliases {
		s.Aliases[AliasToken(a.Kind, a.Name)] = a
	}
	return s
}

func primaryHost(plan PreviewPlan, host string) *PreviewRoute {
	for _, r := range plan.Routes {
		if r.Primary && r.Host == host {
			return &r
		}
	}
	return nil
}

func TestTagFirstPreviewSelection(t *testing.T) {
	branch := Alias{Kind: "branch", Name: "refs/heads/release", Release: "branch"}
	tag := Alias{Kind: "tag", Name: "refs/tags/release", Release: "tag"}
	commit := Alias{Kind: "commit", Name: strings.Repeat("a", 40), Release: "commit"}
	s := previewSite("app", branch, tag, commit)
	plan, err := PlanPreviews([]*Site{s}, "example.com", "flat", PreviewAliasTagFirst)
	if err != nil {
		t.Fatal(err)
	}
	winner := primaryHost(plan, "release--app.example.com")
	if winner == nil || winner.Alias != tag {
		t.Fatalf("tag did not win: %+v", plan)
	}
	if len(plan.Routes) != 4 || len(plan.Problems) != 1 || plan.Problems[0].Kind != "branch" {
		t.Fatalf("legacy/commit routes or diagnostic lost: %+v", plan)
	}
	if r := primaryHost(plan, Host("example.com", "flat", "app", AliasToken("commit", commit.Name))); r == nil || r.Alias != commit {
		t.Fatal("commit URL changed")
	}
	delete(s.Aliases, AliasToken(tag.Kind, tag.Name))
	plan, err = PlanPreviews([]*Site{s}, "example.com", "flat", PreviewAliasTagFirst)
	fallback := primaryHost(plan, "release--app.example.com")
	if err != nil || fallback == nil || fallback.Alias != branch {
		t.Fatalf("branch did not become the fallback: %+v %v", plan, err)
	}
	plan, err = PlanPreviews([]*Site{s}, "example.com", "", "")
	if err != nil || len(plan.Routes) != 2 || len(plan.Problems) != 0 {
		t.Fatalf("default qualified mode changed: %+v %v", plan, err)
	}
	for _, r := range plan.Routes {
		if !r.Primary || r.Host != Host("example.com", "flat", "app", r.Token) {
			t.Fatalf("default qualified mode changed: %+v", r)
		}
	}
}

func TestTagFirstAmbiguityDoesNotFallBackOrDependOnOrder(t *testing.T) {
	for _, aliases := range [][]Alias{
		{{Kind: "tag", Name: "refs/tags/v1.0", Release: "same"}, {Kind: "tag", Name: "refs/tags/v1-0", Release: "same"}, {Kind: "branch", Name: "refs/heads/v1-0", Release: "branch"}},
		{{Kind: "branch", Name: "refs/heads/feat/Foo", Release: "one"}, {Kind: "branch", Name: "refs/heads/feat/foo", Release: "two"}},
	} {
		s := previewSite("app", aliases...)
		first, err := PlanPreviews([]*Site{s}, "example.com", "flat", PreviewAliasTagFirst)
		if err != nil || len(first.Routes) != len(aliases) || len(first.Problems) != len(aliases) {
			t.Fatalf("ambiguous aliases were not preserved only at qualified URLs: %+v %v", first, err)
		}
		for _, route := range first.Routes {
			if route.Primary {
				t.Fatalf("ambiguous short URL became canonical: %+v", route)
			}
		}
		for i := 0; i < 20; i++ {
			next, err := PlanPreviews([]*Site{s}, "example.com", "flat", PreviewAliasTagFirst)
			if err != nil || !reflect.DeepEqual(first, next) {
				t.Fatalf("map order changed resolution: %+v %v", next, err)
			}
		}
	}
}

func TestTagFirstShortNamesAreReadableAndBounded(t *testing.T) {
	for _, tc := range []struct {
		name, kind, style, site, host string
	}{
		{"refs/heads/feat/test", "branch", "flat", "app", "feat-test--app.example.com"},
		{"refs/tags/v0.1.0", "tag", "flat", "app", "v0-1-0--app.example.com"},
		{"refs/tags/v0.1.0", "tag", "nested", "app", "v0-1-0.app.example.com"},
		{"manual name", "branch", "flat", "app", "manual-name--app.example.com"},
		{"refs/tags/manual", "branch", "flat", "app", "refs-tags-manual--app.example.com"},
		{strings.Repeat("long", 30), "tag", "flat", "app", ""},
		{"你好", "tag", "flat", "app", ""},
		{"x", "branch", "flat", strings.Repeat("s", 63), ""},
	} {
		t.Run(tc.name+tc.style+tc.site, func(t *testing.T) {
			s := previewSite(tc.site, Alias{Kind: tc.kind, Name: tc.name})
			plan, err := PlanPreviews([]*Site{s}, "example.com", tc.style, PreviewAliasTagFirst)
			if err != nil {
				t.Fatal(err)
			}
			if tc.host == "" {
				if len(plan.Routes) != 1 || plan.Routes[0].Primary || len(plan.Problems) != 1 {
					t.Fatalf("unsafe short name was hashed/truncated instead of omitted: %+v", plan)
				}
			} else if r := primaryHost(plan, tc.host); r == nil {
				t.Fatalf("missing readable hostname %s: %+v", tc.host, plan)
			}
		})
	}
}

func TestTagFirstCannotTakeProductionOrQualifiedNames(t *testing.T) {
	branch := Alias{Kind: "branch", Name: "refs/heads/dev"}
	for _, disabled := range []bool{false, true} {
		s := previewSite("app", branch)
		production := previewSite("dev--app")
		production.Disabled = disabled
		plan, err := PlanPreviews([]*Site{s, production}, "example.com", "flat", PreviewAliasTagFirst)
		if err != nil || primaryHost(plan, "dev--app.example.com") != nil || len(plan.Problems) != 1 {
			t.Fatalf("production name was stolen (disabled=%v): %+v %v", disabled, plan, err)
		}
	}
	legacy := AliasToken(branch.Kind, branch.Name)
	tag := Alias{Kind: "tag", Name: "refs/tags/" + legacy}
	s := previewSite("app", branch, tag)
	plan, err := PlanPreviews([]*Site{s}, "example.com", "flat", PreviewAliasTagFirst)
	if err != nil || len(plan.Problems) != 1 {
		t.Fatalf("qualified collision not diagnosed: %+v %v", plan, err)
	}
	for _, r := range plan.Routes {
		if r.Host == Host("example.com", "flat", "app", legacy) && (r.Alias != branch || r.Primary) {
			t.Fatalf("tag changed a qualified branch address: %+v", r)
		}
	}
}
