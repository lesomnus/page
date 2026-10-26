package routes

import (
	"errors"
	"strings"
	"sync"
	"testing"
)

func inventory(names ...string) map[string]bool {
	m := map[string]bool{}
	for _, name := range names {
		m[name] = true
	}
	return m
}
func compileJSON(t *testing.T, raw string, files map[string]bool) *Engine {
	t.Helper()
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	e, err := Compile(c, files)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func TestDefaultFileAndIndexBehavior(t *testing.T) {
	e, err := Compile(nil, inventory("index.html", "docs/index.html", "assets/app.js", "about.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		url, path, redirect string
		status              int
	}{
		{"/", "index.html", "", 200}, {"/docs", "", "/docs/", 301}, {"/docs/", "docs/index.html", "", 200}, {"/assets/app.js", "assets/app.js", "", 200}, {"/about", "", "", 404}, {"/missing", "", "", 404}, {"/missing.js", "", "", 404},
	} {
		t.Run(test.url, func(t *testing.T) {
			d := e.Resolve(test.url, false)
			if d.Path != test.path || d.Redirect != test.redirect || d.Status != test.status {
				t.Fatalf("%s: %+v", test.url, d)
			}
		})
	}
}

func TestOrderedRoutingCapturesHeadersAndSPA(t *testing.T) {
	e := compileJSON(t, `{
  "schemaVersion":1,
  "routing":{
    "cleanUrls":true,
    "routes":[
      {"from":"/old","redirect":"/new","status":308},
      {"from":"/profiles/:name","rewrite":"/users/:name.html"},
      {"from":"/docs/*","rewrite":"/docs/index.html"},
      {"from":"/move/*","redirect":"/new/*"},
      {"from":"/one","rewrite":"/two"},
      {"from":"/two","redirect":"/three"}
    ],
    "fallback":{"rewrite":"/index.html","exclude":["/assets/*","/favicon.ico"]},
    "errorPages":{"404":"/404.html"}
  },
  "headers":[
    {"from":"/*","set":{"X-Test":"first","X-Content-Type-Options":"nosniff"}},
    {"from":"/profiles/*","set":{"X-Test":"profile","Content-Security-Policy":"default-src 'self'"}},
    {"from":"/old","set":{"Cache-Control":"no-store"}}
  ]
}`, inventory("index.html", "404.html", "users/alice.html", "docs/index.html", "docs/existing.html", "about.html", "two"))
	for _, test := range []struct {
		url                    string
		navigation             bool
		file, redirect, action string
		status                 int
	}{
		{"/old", true, "", "/new", ActionRedirect, 308},
		{"/profiles/alice", true, "users/alice.html", "", ActionRewrite, 200},
		{"/profiles/bob", true, "404.html", "", ActionRewrite, 404},
		{"/docs/existing.html", true, "docs/index.html", "", ActionRewrite, 200},
		{"/docs/nested/path", true, "docs/index.html", "", ActionRewrite, 200},
		{"/move/a/b", false, "", "/new/a/b", ActionRedirect, 302},
		{"/one", false, "two", "", ActionRewrite, 200},
		{"/two", false, "", "/three", ActionRedirect, 302},
		{"/about", false, "about.html", "", ActionFile, 200},
		{"/client/route", true, "index.html", "", ActionFallback, 200},
		{"/client/route", false, "404.html", "", ActionNotFound, 404},
		{"/assets/missing.js", true, "404.html", "", ActionNotFound, 404},
		{"/favicon.ico", true, "404.html", "", ActionNotFound, 404},
	} {
		t.Run(test.url+test.action, func(t *testing.T) {
			d := e.Resolve(test.url, test.navigation)
			if d.Path != test.file || d.Redirect != test.redirect || d.Status != test.status || d.Action != test.action {
				t.Fatalf("got %+v", d)
			}
			if d.Headers["X-Test"] == "" {
				t.Fatal("missing original-path headers")
			}
		})
	}
	d := e.Resolve("/profiles/alice", true)
	if d.Headers["X-Test"] != "profile" || !strings.Contains(d.Headers["Content-Security-Policy"], "'self'") {
		t.Fatalf("header precedence: %+v", d.Headers)
	}
	if e.Resolve("/old", false).Headers["Cache-Control"] != "no-store" {
		t.Fatal("redirect headers lost")
	}
	if e.Resolve("/assets/no.js", false).Headers["X-Test"] != "first" {
		t.Fatal("404 headers lost")
	}
}

func TestTrailingSlashPolicies(t *testing.T) {
	files := inventory("index.html", "docs/index.html", "about.html", "app.js")
	for _, mode := range []string{"append", "remove", "preserve"} {
		t.Run(mode, func(t *testing.T) {
			e, err := Compile(&Config{SchemaVersion: 1, Routing: Routing{CleanURLs: true, TrailingSlash: mode}}, files)
			if err != nil {
				t.Fatal(err)
			}
			for _, url := range []string{"/docs", "/docs/", "/about", "/about/"} {
				d := e.Resolve(url, false)
				want := url
				if mode == "append" && !strings.HasSuffix(url, "/") {
					want = url + "/"
				}
				if mode == "remove" && strings.HasSuffix(url, "/") {
					want = strings.TrimSuffix(url, "/")
				}
				if mode == "preserve" && url == "/docs" {
					want = "/docs/"
				}
				if want != url {
					if d.Redirect != want {
						t.Fatalf("%s: %+v", url, d)
					}
					if following := e.Resolve(want, false); following.Status != 200 || following.Redirect != "" {
						t.Fatalf("normalization loop: %+v", following)
					}
				} else if d.Status != 200 {
					t.Fatalf("%s: %+v", url, d)
				}
			}
			if d := e.Resolve("/app.js", false); d.Status != 200 || d.Redirect != "" {
				t.Fatal("file URL normalized")
			}
			if d := e.Resolve("/", false); d.Status != 200 || d.Redirect != "" {
				t.Fatal("root normalized")
			}
		})
	}
}

func TestRejectInvalidConfiguration(t *testing.T) {
	files := inventory("index.html", "404.html", "docs/index.html", "users/alice.html", "assets/app.js", "assets/page.HTML")
	tests := []string{
		`{"schemaVersion":2}`,
		`{"schemaVersion":1,"unknown":true}`,
		`{"schemaVersion":1} {"schemaVersion":1}`,
		`{"schemaVersion":1,"routing":{"cleanUrl":true}}`,
		`{"schemaVersion":1,"routing":{"index":"../index.html"}}`,
		`{"schemaVersion":1,"routing":{"trailingSlash":"invalid"}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","rewrite":"/missing"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x/:id","rewrite":"/missing/:id"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","rewrite":"/index.html","redirect":"/index.html"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"https://example.com"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"//example.com"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"/%2e%2e/x"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"/x?query=1"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"/y","status":200}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","rewrite":"/index.html","status":201}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x/:id/:id","redirect":"/index.html"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x/*/y","redirect":"/index.html"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x/:id","redirect":"/users/:unknown"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/x","redirect":"/y"},{"from":"/x","redirect":"/z"}]}}`,
		`{"schemaVersion":1,"routing":{"fallback":{"rewrite":"/missing"}}}`,
		`{"schemaVersion":1,"routing":{"errorPages":{"500":"/404.html"}}}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"Set-Cookie":"sensitive=1"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"Content-Length":"1"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"Content-Type":"text/html"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"Bad Name":"value"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Value":"x\r\nInjected: yes"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"X-Content-Type-Options":"off"}}]}`,
		`{"schemaVersion":1,"headers":[{"from":"/*","set":{"cache-control":"a","Cache-Control":"b"}}]}`,
		`{"schemaVersion":1,"assets":{"immutable":["/assets/*"]}}`,
		`{"schemaVersion":1,"assets":{"immutable":["/*"]}}`,
	}
	for _, raw := range tests {
		t.Run(raw, func(t *testing.T) {
			c, err := Parse([]byte(raw))
			if err == nil {
				_, err = Compile(c, files)
			}
			if !errors.Is(err, ErrInvalid) {
				t.Fatalf("accepted config/error: %v", err)
			}
		})
	}
	if _, err := Parse([]byte(strings.Repeat(" ", MaxConfigBytes) + `{"schemaVersion":1}`)); !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized JSON accepted")
	}
	tooMany := &Config{SchemaVersion: 1}
	for i := 0; i < MaxRules+1; i++ {
		tooMany.Headers = append(tooMany.Headers, HeaderRule{From: "/*", Set: map[string]string{"X-Test": "yes"}})
	}
	if _, err := Compile(tooMany, files); !errors.Is(err, ErrInvalid) {
		t.Fatal("rule limit ignored")
	}
}

func TestRedirectCycleValidation(t *testing.T) {
	for _, raw := range []string{
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/a","redirect":"/a"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/a","redirect":"/b"},{"from":"/b","redirect":"/a"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/a/:id","redirect":"/b/:id"},{"from":"/b/:id","redirect":"/a/:id"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/docs/","redirect":"/docs"}]}}`,
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/a/*","redirect":"/a/more/*"}]}}`,
		// The cycle requires x=a. Sampling a capture named "sample" misses it.
		`{"schemaVersion":1,"routing":{"routes":[{"from":"/:x/a","redirect":"/b/:x"},{"from":"/b/:y","redirect":"/:y/:y"}]}}`,
	} {
		c, err := Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		if _, err = Compile(c, inventory("docs/index.html")); !errors.Is(err, ErrInvalid) {
			t.Fatalf("accepted redirect cycle %s: %v", raw, err)
		}
	}
}

func TestRedirectDependencyRespectsEarlierRewrite(t *testing.T) {
	e := compileJSON(t, `{"schemaVersion":1,"routing":{"routes":[{"from":"/done","rewrite":"/index.html"},{"from":"/*","redirect":"/done"}]}}`, inventory("index.html"))
	if got := e.Resolve("/elsewhere", false); got.Redirect != "/done" || got.Status != 302 {
		t.Fatalf("redirect: %+v", got)
	}
	if got := e.Resolve("/done", false); got.Path != "index.html" || got.Status != 200 {
		t.Fatalf("rewrite did not terminate redirect: %+v", got)
	}
}

func TestImmutableAssetsAndConcurrentUse(t *testing.T) {
	c := &Config{SchemaVersion: 1, Assets: Assets{Immutable: []string{"/assets/*"}}, Headers: []HeaderRule{{From: "/*", Set: map[string]string{"X-Test": "original"}}}}
	e, err := Compile(c, inventory("index.html", "assets/app.abc.js"))
	if err != nil {
		t.Fatal(err)
	}
	c.Headers[0].Set["X-Test"] = "mutated"
	c.Assets.Immutable[0] = "/*"
	if !e.Immutable("assets/app.abc.js") || !e.Immutable("/assets/app.abc.js") || e.Immutable("index.html") || e.Immutable("../assets/x") {
		t.Fatal("wrong immutable matching")
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Go(func() {
			for j := 0; j < 100; j++ {
				d := e.Resolve("/assets/app.abc.js", false)
				if d.Headers["X-Test"] != "original" {
					t.Error("compile retained mutable config")
				}
				d.Headers["X-Test"] = "caller mutation"
			}
		})
	}
	wg.Wait()
}

func TestUnsafeRequestPaths(t *testing.T) {
	e, err := Compile(nil, inventory("index.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"../index.html", "//evil", "/a/../index.html", "/a//x", "/%2e%2e/x", "/a\\x", "/a\x00x", "/a?b"} {
		if d := e.Resolve(p, true); d.Status != 400 {
			t.Fatalf("unsafe path %q: %+v", p, d)
		}
	}
}
