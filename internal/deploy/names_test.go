package deploy

import (
	"strings"
	"testing"
)

func TestPreviewNames(t *testing.T) {
	a := AliasToken("branch", "feature/login")
	if !ValidName(a) || a != AliasToken("branch", "feature/login") {
		t.Fatalf("unstable or invalid token %q", a)
	}
	for _, b := range []string{AliasToken("branch", "feature-login"), AliasToken("tag", "feature/login"), AliasToken("branch", "FEATURE/login")} {
		if a == b {
			t.Fatalf("colliding source names: %q", a)
		}
	}
	for _, name := range []string{"v1.2.0", "你好", strings.Repeat("a/", 1000)} {
		token := AliasToken("tag", name)
		if !ValidName(token) || len(token) > 40 {
			t.Fatalf("unbounded alias %q", token)
		}
	}
	if got := Host("example.com", "flat", "foo", ""); got != "foo.example.com" {
		t.Fatal(got)
	}
	if got := Host("example.com", "nested", "foo", a); got != a+".foo.example.com" {
		t.Fatal(got)
	}
	flat := Host("example.com", "flat", strings.Repeat("s", 63), a)
	if !ValidName(strings.TrimSuffix(flat, ".example.com")) {
		t.Fatalf("invalid long preview host %q", flat)
	}
	if flat == Host("example.com", "flat", strings.Repeat("s", 62)+"t", a) {
		t.Fatal("long site hosts collided")
	}
}
