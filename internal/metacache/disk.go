package metacache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	diskVersion        = 1
	maxDataBytes       = 16 << 20
	maxDiskRecordBytes = (maxDataBytes*4+2)/3 + (64 << 10)
)

type diskRecord struct {
	Version   int    `json:"version"`
	Namespace string `json:"namespace"`
	Key       string `json:"key"`
	Mutable   bool   `json:"mutable"`
	ETag      string `json:"etag"`
	Data      []byte `json:"data"`
	Checksum  string `json:"checksum"`
}

func digest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func (c *Cache) diskPath(key string) string {
	return filepath.Join(c.directory, digest([]byte(key))+".json")
}
func diskWeight(size int64) int64 { return ((size + 4095) / 4096) * 4096 }
func ownedFile(name string) bool {
	if len(name) != 64+len(".json") || !strings.HasSuffix(name, ".json") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimSuffix(name, ".json"))
	return err == nil
}
func recordChecksum(r diskRecord) string {
	r.Checksum = ""
	encoded, _ := json.Marshal(r)
	return digest(encoded)
}
func (c *Cache) openDisk() error {
	c.directory = filepath.Join(c.config.Directory, digest([]byte(c.config.Namespace)))
	if err := os.MkdirAll(c.directory, 0700); err != nil {
		return err
	}
	if err := os.Chmod(c.directory, 0700); err != nil {
		return err
	}
	return c.pruneDisk(0)
}

// Disk records are disposable, local to this process and cache namespace. No
// persistent in-memory directory index is needed; pruning scans only on pressure.
func (c *Cache) pruneDisk(incoming int64) error {
	entries, err := os.ReadDir(c.directory)
	if err != nil {
		return err
	}
	type candidate struct {
		path     string
		size     int64
		modified time.Time
	}
	var files []candidate
	used := int64(0)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".metadata-") {
			if err := os.Remove(filepath.Join(c.directory, entry.Name())); err != nil && !os.IsNotExist(err) {
				return err
			}
			continue
		}
		if !ownedFile(entry.Name()) {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		size := diskWeight(info.Size())
		used += size
		files = append(files, candidate{filepath.Join(c.directory, entry.Name()), size, info.ModTime()})
	}
	c.diskUsed = used
	if used+incoming <= c.config.DiskBytes {
		return nil
	}
	sort.Slice(files, func(i, j int) bool { return files[i].modified.Before(files[j].modified) })
	// Reclaim a small batch to avoid scanning the entire directory on every miss.
	target := c.config.DiskBytes - c.config.DiskBytes/10
	for _, file := range files {
		if c.diskUsed+incoming <= target {
			break
		}
		if err := os.Remove(file.path); err != nil && !os.IsNotExist(err) {
			return err
		}
		c.diskUsed -= file.size
	}
	return nil
}

func (c *Cache) readDisk(key string, mutable bool) ([]byte, string, bool) {
	if c.directory == "" {
		return nil, "", false
	}
	path := c.diskPath(key)
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() < 0 || info.Size() > maxDiskRecordBytes || diskWeight(info.Size()) > c.config.DiskBytes {
		return nil, "", false
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, "", false
	}
	data, err := io.ReadAll(io.LimitReader(file, min(c.config.DiskBytes, int64(maxDiskRecordBytes))+1))
	file.Close()
	if err != nil || int64(len(data)) > min(c.config.DiskBytes, int64(maxDiskRecordBytes)) {
		return nil, "", false
	}
	var r diskRecord
	if err := json.Unmarshal(data, &r); err != nil || r.Version != diskVersion || r.Namespace != c.config.Namespace || r.Key != key || r.Mutable != mutable || len(r.Data) > maxDataBytes || r.Checksum != recordChecksum(r) {
		c.diskMu.Lock()
		if current, err := os.Lstat(path); err == nil && os.SameFile(info, current) {
			if err := os.Remove(path); err == nil {
				// An externally corrupted file may have changed its size. Recount
				// disposable records rather than subtracting an outdated allocation.
				if err := c.pruneDisk(0); err != nil {
					c.warn(err)
				}
			}
		}
		c.diskMu.Unlock()
		return nil, "", false
	}
	now := c.config.Now()
	_ = os.Chtimes(path, now, now)
	return r.Data, r.ETag, true
}

func (c *Cache) writeDisk(key string, mutable bool, r *record) {
	if c.directory == "" || len(r.data) > maxDataBytes {
		return
	}
	record := diskRecord{Version: diskVersion, Namespace: c.config.Namespace, Key: key, Mutable: mutable, ETag: r.etag, Data: r.data}
	record.Checksum = recordChecksum(record)
	encoded, err := json.Marshal(record)
	if err != nil {
		c.warn(err)
		return
	}
	weight := diskWeight(int64(len(encoded)))
	if weight > c.config.DiskBytes || len(encoded) > maxDiskRecordBytes {
		return
	}
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	path := c.diskPath(key)
	oldWeight := int64(0)
	if old, err := os.Lstat(path); err == nil && old.Mode().IsRegular() {
		oldWeight = diskWeight(old.Size())
	}
	if oldWeight > c.diskUsed {
		if err := c.pruneDisk(0); err != nil {
			c.warn(err)
			return
		}
		oldWeight = 0
		if old, err := os.Lstat(path); err == nil && old.Mode().IsRegular() {
			oldWeight = diskWeight(old.Size())
		}
	}
	if c.diskUsed-oldWeight+weight > c.config.DiskBytes {
		if err := c.pruneDisk(weight); err != nil {
			c.warn(err)
			return
		}
		oldWeight = 0
		if old, err := os.Lstat(path); err == nil && old.Mode().IsRegular() {
			oldWeight = diskWeight(old.Size())
		}
	}
	file, err := os.CreateTemp(c.directory, ".metadata-*")
	if err != nil {
		c.warn(err)
		return
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if _, err = file.Write(encoded); err == nil {
		err = file.Close()
	} else {
		_ = file.Close()
	}
	if err != nil {
		c.warn(err)
		return
	}
	now := c.config.Now()
	_ = os.Chtimes(temporary, now, now)
	// Only the atomic rename is under the memory lock. Invalidation cannot be
	// overtaken by an old in-flight read replacing a newer disk generation.
	c.mu.Lock()
	if mutable && r.generation != c.generation {
		c.mu.Unlock()
		return
	}
	err = os.Rename(temporary, path)
	c.mu.Unlock()
	if err != nil {
		c.warn(err)
		return
	}
	c.diskUsed = max(0, c.diskUsed-oldWeight) + weight
}
func (c *Cache) removeDisk(key string, mutable bool, generation uint64) {
	if c.directory == "" {
		return
	}
	c.diskMu.Lock()
	defer c.diskMu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	if mutable && generation != c.generation {
		return
	}
	path := c.diskPath(key)
	info, err := os.Lstat(path)
	if err != nil {
		return
	}
	if err := os.Remove(path); err != nil {
		if !os.IsNotExist(err) {
			c.warn(fmt.Errorf("remove disk record: %w", err))
		}
		return
	}
	if info.Mode().IsRegular() {
		c.diskUsed = max(0, c.diskUsed-diskWeight(info.Size()))
	}
}
