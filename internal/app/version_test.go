package app

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the same linker field as the container build, using a real binary.
// A version query must also work when deployment configuration is unavailable.
func TestStampedVersionWithoutConfiguration(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "page")
	build := exec.Command("go", "build", "-buildvcs=false", "-ldflags=-X github.com/lesomnus/payday/version.version=v1.2.3-test", "-o", binary, "../../cmd/page")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build stamped binary: %v\n%s", err, out)
	}
	query := exec.Command(binary, "--config", filepath.Join(dir, "missing.yaml"), "version")
	out, err := query.CombinedOutput()
	if err != nil {
		t.Fatalf("query stamped binary: %v\n%s", err, out)
	}
	if strings.TrimSpace(string(out)) != "page v1.2.3-test" {
		t.Fatalf("wrong stamped version: %q", out)
	}
}
