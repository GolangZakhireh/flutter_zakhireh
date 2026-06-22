package storage

import (
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// FSBackend implements Backend using the local filesystem
type FSBackend struct {
	Root string
}

// NewFSBackend creates a new filesystem storage backend rooted at the given directory.
func NewFSBackend(root string) *FSBackend {
	return &FSBackend{Root: root}
}

// Init creates the root directory if it does not exist.
func (s *FSBackend) Init() error {
	return os.MkdirAll(s.Root, 0755)
}

// packageDir returns the path to a package's directory (e.g. <root>/<package>).
func (s *FSBackend) packageDir(packageName string) (string, error) {
	if err := validatePathSegment(packageName); err != nil {
		return "", err
	}
	return filepath.Join(s.Root, packageName), nil
}

// filePath returns the full path to a specific package file (e.g. <root>/<package>/<version>.tar.gz).
func (s *FSBackend) filePath(packageName, version, ext string) (string, error) {
	if err := validatePathSegment(packageName); err != nil {
		return "", err
	}
	if err := validatePathSegment(version); err != nil {
		return "", err
	}
	// ext is trusted as it's hardcoded in callers, but good to check
	if strings.Contains(ext, "/") || strings.Contains(ext, "\\") {
		return "", fmt.Errorf("invalid extension")
	}

	dir, err := s.packageDir(packageName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, version+ext), nil
}

// metadataPath returns the path to the package metadata file (e.g. <root>/<package>.json).
func (s *FSBackend) metadataPath(packageName string) (string, error) {
	if err := validatePathSegment(packageName); err != nil {
		return "", err
	}
	return filepath.Join(s.Root, packageName+".json"), nil
}

// validatePathSegment checks that a path segment is safe (no traversal, no absolute paths).
func validatePathSegment(segment string) error {
	if segment == "" {
		return fmt.Errorf("empty path segment")
	}
	if strings.Contains(segment, "..") {
		return fmt.Errorf("path traversal attempt")
	}
	if filepath.IsAbs(segment) {
		return fmt.Errorf("absolute path not allowed")
	}
	return nil
}

// Exists checks whether a specific package file exists in local storage.
func (s *FSBackend) Exists(packageName, version, ext string) (bool, error) {
	path, err := s.filePath(packageName, version, ext)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Get opens a package file for reading.
func (s *FSBackend) Get(packageName, version, ext string) (io.ReadCloser, error) {
	path, err := s.filePath(packageName, version, ext)
	if err != nil {
		return nil, err
	}
	return os.Open(path)
}

// Save writes a package file to local storage, creating directories as needed.
func (s *FSBackend) Save(packageName, version, ext string, content io.Reader) error {
	dir, err := s.packageDir(packageName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, version+ext)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = io.Copy(f, content)
	if err != nil {
		fmt.Printf("[Storage] Failed to write file %s: %v\n", path, err)
	}
	return err
}

// SaveMetadata saves package metadata (versions list) to a JSON file.
func (s *FSBackend) SaveMetadata(packageName string, versions []string) error {
	// This is a simplified implementation - in production you'd use encoding/json
	// For now, we'll store as a simple text file with one version per line
	dir, err := s.packageDir(packageName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}

	path := filepath.Join(dir, "versions.txt")
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	for _, v := range versions {
		_, err = f.WriteString(v + "\n")
		if err != nil {
			return err
		}
	}
	return nil
}

// GetMetadata retrieves package metadata (versions list).
func (s *FSBackend) GetMetadata(packageName string) ([]string, error) {
	path, err := s.metadataPath(packageName)
	if err != nil {
		return nil, err
	}
	
	content, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	
	// Parse versions (one per line)
	lines := strings.Split(strings.TrimSpace(string(content)), "\n")
	var versions []string
	for _, line := range lines {
		if line != "" {
			versions = append(versions, line)
		}
	}
	return versions, nil
}

// ListVersions returns all cached version strings for a package.
func (s *FSBackend) ListVersions(packageName string) ([]string, error) {
	// Try to get from metadata file first
	versions, err := s.GetMetadata(packageName)
	if err == nil && len(versions) > 0 {
		return versions, nil
	}
	
	// Fallback: scan directory for .tar.gz files
	dir, err := s.packageDir(packageName)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}

	var versionsList []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tar.gz") {
			versionsList = append(versionsList, strings.TrimSuffix(name, ".tar.gz"))
		}
	}
	return versionsList, nil
}

// packageMetaFile returns the path to a package's metadata file.
// Package (all-versions) metadata: <root>/<package>/package.meta.json
// Version metadata:                <root>/<package>/<version>.meta.json
func (s *FSBackend) metaFile(packageName, version string) (string, error) {
	dir, err := s.packageDir(packageName)
	if err != nil {
		return "", err
	}
	name := "package.meta.json"
	if version != "" {
		name = version + ".meta.json"
	}
	return filepath.Join(dir, name), nil
}

// SavePackageMeta stores the raw package (all-versions) metadata blob.
func (s *FSBackend) SavePackageMeta(packageName string, data []byte) error {
	dir, err := s.packageDir(packageName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	path, err := s.metaFile(packageName, "")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// GetPackageMeta reads the cached package (all-versions) metadata blob.
func (s *FSBackend) GetPackageMeta(packageName string) ([]byte, bool, error) {
	path, err := s.metaFile(packageName, "")
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// SaveVersionMeta stores the raw version metadata blob.
func (s *FSBackend) SaveVersionMeta(packageName, version string, data []byte) error {
	dir, err := s.packageDir(packageName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	path, err := s.metaFile(packageName, version)
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// GetVersionMeta reads a cached version metadata blob.
func (s *FSBackend) GetVersionMeta(packageName, version string) ([]byte, bool, error) {
	path, err := s.metaFile(packageName, version)
	if err != nil {
		return nil, false, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}

// Walk iterates over all cached packages, calling fn for each one found.
func (s *FSBackend) Walk(fn func(pkg Package) error) error {
	return filepath.WalkDir(s.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		// Skip the root directory
		if path == s.Root {
			return nil
		}

		// Look for package directories (not files)
		if !d.IsDir() {
			return nil
		}

		// Check if this is a package directory (contains .tar.gz files)
		relPath, _ := filepath.Rel(s.Root, path)
		packageName := relPath

		p := Package{
			Name: packageName,
		}

		files, err := os.ReadDir(path)
		if err != nil {
			return nil // skip unreadable directories
		}

		for _, f := range files {
			if strings.HasSuffix(f.Name(), ".tar.gz") {
				version := strings.TrimSuffix(f.Name(), ".tar.gz")
				p.Versions = append(p.Versions, version)
				info, err := f.Info()
				if err == nil {
					p.Size += info.Size()
					if info.ModTime().After(p.UpdatedAt) {
						p.UpdatedAt = info.ModTime()
					}
				}
			}
		}

		if len(p.Versions) > 0 {
			if err := fn(p); err != nil {
				return err
			}
		}

		return nil
	})
}
