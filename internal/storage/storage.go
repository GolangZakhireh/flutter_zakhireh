package storage

import (
	"io"
	"time"
)

// Package represents a cached Flutter package with statistics
type Package struct {
	Name      string
	Versions  []string
	Size      int64
	UpdatedAt time.Time
}

// Backend defines the storage interface for Flutter packages
type Backend interface {
	// Initialize the storage (e.g. ensure directories exist)
	Init() error

	// Exists checks if a specific package file exists
	// ext should include the dot, e.g. ".tar.gz", ".json"
	Exists(packageName, version, ext string) (bool, error)

	// Get opens a package file for reading
	Get(packageName, version, ext string) (io.ReadCloser, error)

	// Save writes content to a package file
	Save(packageName, version, ext string, content io.Reader) error

	// SaveMetadata saves package metadata (versions list)
	SaveMetadata(packageName string, versions []string) error

	// ListVersions returns all known versions for a package
	ListVersions(packageName string) ([]string, error)

	// Walk iterates over all stored packages
	Walk(fn func(pkg Package) error) error

	// SavePackageMeta stores the raw package metadata blob (full pub.dev
	// /api/packages/<name> JSON, with archive_urls already rewritten).
	SavePackageMeta(packageName string, data []byte) error

	// GetPackageMeta reads the cached package metadata blob.
	// Returns (nil, false, nil) when no metadata is cached.
	GetPackageMeta(packageName string) ([]byte, bool, error)

	// SaveVersionMeta stores the raw version metadata blob (full pub.dev
	// /api/packages/<name>/versions/<version> JSON).
	SaveVersionMeta(packageName, version string, data []byte) error

	// GetVersionMeta reads a cached version metadata blob.
	// Returns (nil, false, nil) when no metadata is cached.
	GetVersionMeta(packageName, version string) ([]byte, bool, error)
}
