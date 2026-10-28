package app

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestMetadataCacheConfiguration(t *testing.T) {
	c, err := Load(configFile(t, "base_domain: example.com\n"))
	if err != nil {
		t.Fatal(err)
	}
	if c.MetadataCache.TTL != 5*time.Second || c.MetadataCache.MemoryBytes != 4<<20 || c.MetadataCache.DiskBytes != 64<<20 || c.MetadataCache.Directory != filepath.Join(os.TempDir(), "page-cache") {
		t.Fatalf("wrong defaults: %+v", c.MetadataCache)
	}
	c, err = Load(configFile(t, "metadata_cache:\n  directory: ''\n  ttl: 2s\n  memory_bytes: 1048576\n  disk_bytes: 8388608\n"))
	if err != nil || c.MetadataCache.Directory != "" || c.MetadataCache.TTL != 2*time.Second || c.MetadataCache.MemoryBytes != 1<<20 || c.MetadataCache.DiskBytes != 8<<20 {
		t.Fatalf("wrong override: %+v %v", c.MetadataCache, err)
	}
	for _, raw := range []string{"metadata_cache:\n  ttl: -1s\n", "metadata_cache:\n  memory_bytes: -1\n", "metadata_cache:\n  disk_bytes: -1\n", "metadata_cache:\n  unknown: true\n"} {
		if _, err := Load(configFile(t, raw)); err == nil {
			t.Fatalf("invalid cache configuration accepted: %s", raw)
		}
	}
}
