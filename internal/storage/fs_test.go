package storage

import (
	"io"
	"os"
	"strings"
	"testing"
)

func TestFSBackend_SaveValues(t *testing.T) {
	tmpDir, err := os.MkdirTemp("", "flutterzakhireh-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(tmpDir)

	store := NewFSBackend(tmpDir)
	if err := store.Init(); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	packageName := "example_package"
	version := "1.0.0"
	content := "some content"

	// 1. Save valid file
	err = store.Save(packageName, version, ".tar.gz", strings.NewReader(content))
	if err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	// 2. Check Exists
	exists, err := store.Exists(packageName, version, ".tar.gz")
	if err != nil {
		t.Fatalf("Exists failed: %v", err)
	}
	if !exists {
		t.Error("expected file to exist")
	}

	// 3. Get content
	rc, err := store.Get(packageName, version, ".tar.gz")
	if err != nil {
		t.Fatalf("Get failed: %v", err)
	}
	defer rc.Close()

	data, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if string(data) != content {
		t.Errorf("expected content %q, got %q", content, string(data))
	}
}

func TestFSBackend_Validation(t *testing.T) {
	store := NewFSBackend("./tmp")

	tests := []struct {
		name     string
		package_ string
		version  string
		wantErr  bool
	}{
		{"valid", "example_package", "1.0.0", false},
		{"traversal in package", "../secrets", "1.0.0", true},
		{"traversal in version", "example_package", "../../etc/passwd", true},
		{"absolute path", "/etc/local", "1.0.0", true},
		{"empty", "", "1.0.0", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := store.filePath(tt.package_, tt.version, ".tar.gz")
			if (err != nil) != tt.wantErr {
				t.Errorf("filePath() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
