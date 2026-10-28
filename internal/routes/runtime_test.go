package routes

import (
	"errors"
	"reflect"
	"testing"
)

func TestRuntimeResolutionMatchesInventoryResolution(t *testing.T) {
	c, err := Parse([]byte(`{"schemaVersion":1,"routing":{"cleanUrls":true,"routes":[{"from":"/old","redirect":"/about","status":308},{"from":"/profile/:name","rewrite":"/users/:name.html"}],"fallback":{"rewrite":"/index.html","exclude":["/assets/*"]},"errorPages":{"404":"/404.html"}},"headers":[{"from":"/*","set":{"X-Test":"all"}},{"from":"/profile/*","set":{"X-Test":"profile"}}],"assets":{"immutable":["/assets/*"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	files := inventory("index.html", "404.html", "users/alice.html", "docs/index.html", "about.html", "assets/app.js")
	static, err := Compile(c, files)
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := CompileRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.files != nil {
		t.Fatal("runtime compilation retained a file inventory")
	}
	for _, mode := range []string{"append", "remove", "preserve"} {
		c.Routing.TrailingSlash = mode
		static, err = Compile(c, files)
		if err != nil {
			t.Fatal(err)
		}
		runtime, err = CompileRuntime(c)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range []string{"/", "/old", "/profile/alice", "/profile/missing", "/docs", "/docs/", "/about", "/about/", "/assets/app.js", "/assets/missing.js", "/client/deep", "/bad/../path"} {
			for _, navigation := range []bool{false, true} {
				got, err := runtime.ResolveLookup(p, navigation, func(p string) (bool, error) { return files[p], nil })
				if err != nil || !reflect.DeepEqual(got, static.Resolve(p, navigation)) {
					t.Fatalf("%s %s navigation=%v: %+v err=%v; want %+v", mode, p, navigation, got, err, static.Resolve(p, navigation))
				}
			}
		}
	}
	if !runtime.Immutable("assets/app.js") || runtime.Immutable("index.html") {
		t.Fatal("runtime immutable rules were not compiled")
	}
}

func TestRuntimeLooksUpOnlyCandidatesAndPropagatesFailures(t *testing.T) {
	c := &Config{SchemaVersion: 1, Routing: Routing{CleanURLs: true, Fallback: &Fallback{Rewrite: "/index.html"}, ErrorPages: map[string]string{"404": "/404.html"}}}
	e, err := CompileRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	lookup := func(p string) (bool, error) {
		calls = append(calls, p)
		return p == "index.html" || p == "404.html", nil
	}
	d, err := e.ResolveLookup("/client", true, lookup)
	if err != nil || d.Action != ActionFallback || d.Path != "index.html" {
		t.Fatalf("fallback: %+v %v", d, err)
	}
	if !reflect.DeepEqual(calls, []string{"client", "client.html", "client/index.html", "index.html"}) {
		t.Fatalf("unrelated file paths read: %v", calls)
	}
	failure := errors.New("Storage unavailable")
	for _, failed := range calls {
		_, err := e.ResolveLookup("/client", true, func(p string) (bool, error) {
			if p == failed {
				return false, failure
			}
			return false, nil
		})
		if !errors.Is(err, failure) {
			t.Fatalf("lookup %s failure swallowed: %v", failed, err)
		}
	}
	_, err = e.ResolveLookup("/missing.js", false, func(p string) (bool, error) {
		if p == "404.html" {
			return false, failure
		}
		return false, nil
	})
	if !errors.Is(err, failure) {
		t.Fatalf("error document failure swallowed: %v", err)
	}
}

func TestRuntimeCompilationKeepsStructuralValidation(t *testing.T) {
	for _, raw := range []string{
		`{"schemaVersion":1,"routing":{"index":"../index.html"}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"https://example.com"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x/:id","rewrite":"/users/:other"}]}}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"Set-Cookie":"secret"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Test":"bad\r\nvalue"}}]}`,
		`{"schemaVersion":1,"routing":{"errorPages":{"500":"/500.html"}}}`,
	} {
		c, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := CompileRuntime(c); !errors.Is(err, ErrInvalid) {
			t.Fatalf("unsafe runtime config accepted: %s: %v", raw, err)
		}
	}
	c := &Config{SchemaVersion: 1, Routing: Routing{Fallback: &Fallback{Rewrite: "/missing.html"}}}
	if _, err := Compile(c, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("publisher stopped validating inventory")
	}
	e, err := CompileRuntime(c)
	if err != nil {
		t.Fatal(err)
	}
	d, err := e.ResolveLookup("/missing", true, func(string) (bool, error) { return false, nil })
	if err != nil || d.Status != 404 || d.Path != "" {
		t.Fatalf("absent runtime fallback: %+v %v", d, err)
	}
}

func TestBooleanPatternMatchingMatchesCaptureMatcher(t *testing.T) {
	for _, raw := range []string{"/", "/*", "/assets/*", "/:name", "/docs/", "/users/:id/*", "/a/:x/:y", "/a/b/c"} {
		p, err := newPattern(raw)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"/", "/a", "/a/", "/a/b", "/a/b/", "/a/b/c", "/assets/x", "/assets", "/assets/", "/users/alice", "/users/alice/path", "/docs", "/docs/"} {
			_, want := p.match(path)
			if got := matchesPath(p, path); got != want {
				t.Fatalf("pattern %s path %s: %v, want %v", raw, path, got, want)
			}
		}
	}
}

func TestMatchImmutableCompilesOnlyAssetPolicy(t *testing.T) {
	c := &Config{SchemaVersion: 1, Assets: Assets{Immutable: []string{"/assets/*"}}}
	for _, path := range []string{"assets/app.js", "/assets/nested/app.css"} {
		if immutable, err := MatchImmutable(c, path); err != nil || !immutable {
			t.Fatalf("%s: %v %v", path, immutable, err)
		}
	}
	if immutable, err := MatchImmutable(c, "index.html"); err != nil || immutable {
		t.Fatalf("unmatched file: %v %v", immutable, err)
	}
	c.Assets.Immutable = append(c.Assets.Immutable, "/bad/*/path")
	if _, err := MatchImmutable(c, "assets/app.js"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("malformed policy ignored after earlier match: %v", err)
	}
}
