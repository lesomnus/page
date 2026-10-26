// Package storage defines the object operations shared by publishers and serving.
// A successful Put replaces one complete object. It is not a transaction across
// keys and does not arbitrate concurrent publishers.
package storage

import (
	"context"
	"errors"
	"io"
	"strings"
	"time"
)

var (
	ErrNotFound   = errors.New("object not found")
	ErrInvalidKey = errors.New("invalid object key")
)

type Info struct {
	Key         string
	Size        int64
	ContentType string
	ETag        string
	Modified    time.Time
}

type Page struct {
	Objects    []Info
	NextCursor string
}

type Store interface {
	Get(context.Context, string) (io.ReadCloser, Info, error)
	// GetRange reads length bytes beginning at off; both must be nonnegative
	// and length must be positive. Info.Size describes the returned body.
	GetRange(context.Context, string, int64, int64) (io.ReadCloser, Info, error)
	Stat(context.Context, string) (Info, error)
	Put(context.Context, string, io.Reader, int64, string) (Info, error)
	List(context.Context, string, string, int) (Page, error)
	Delete(context.Context, string) error
}

func validKey(key string) error {
	if key == "" || strings.HasPrefix(key, "/") || strings.ContainsAny(key, "\\\x00\r\n") {
		return ErrInvalidKey
	}
	for _, part := range strings.Split(key, "/") {
		if part == "" || part == "." || part == ".." {
			return ErrInvalidKey
		}
	}
	return nil
}

func validPrefix(prefix string) error {
	if prefix == "" {
		return nil
	}
	return validKey(strings.TrimSuffix(prefix, "/"))
}
