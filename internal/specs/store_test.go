package specs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s := NewStore(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	return s
}

func testSpec(id string) *Spec {
	now := time.Now().UTC()
	return &Spec{
		ID:               id,
		OriginalFilename: "app.yaml",
		Name:             "my_app",
		CurrentHash:      "abc123",
		Status:           StatusUploaded,
		UploadedAt:       now,
		UpdatedAt:        now,
	}
}

func TestStoreSaveLoadList(t *testing.T) {
	s := testStore(t)
	a, b := testSpec("spec_a1"), testSpec("spec_b2")
	b.UploadedAt = a.UploadedAt.Add(time.Second)
	for _, sp := range []*Spec{a, b} {
		if err := s.EnsureSpecDirs(sp.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(sp); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Load("spec_a1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "my_app" || got.CurrentHash != "abc123" {
		t.Errorf("unexpected record: %+v", got)
	}
	list, err := s.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != "spec_a1" {
		t.Errorf("expected ordered list of 2, got %v", list)
	}
	if _, err := s.Load("../escape"); err == nil {
		t.Error("expected traversal id to be rejected")
	}
}

func TestStoreVersioning(t *testing.T) {
	s := testStore(t)
	sp := testSpec("spec_v1")
	if err := s.EnsureSpecDirs(sp.ID); err != nil {
		t.Fatal(err)
	}
	data := []byte("name: my_app\ndependencies:\n  http: ^1.1.0\n")
	if err := s.WriteVersion(sp.ID, "hash1", data); err != nil {
		t.Fatal(err)
	}
	src, err := s.ReadSource(sp.ID)
	if err != nil || string(src) != string(data) {
		t.Fatalf("source mismatch: %v", err)
	}
	v, err := s.ReadVersion(sp.ID, "hash1")
	if err != nil || string(v) != string(data) {
		t.Fatalf("version mismatch: %v", err)
	}
}

func TestStoreStagingPromote(t *testing.T) {
	s := testStore(t)
	sp := testSpec("spec_p1")
	if err := s.EnsureSpecDirs(sp.ID); err != nil {
		t.Fatal(err)
	}
	// Seed a good output.
	staging, err := s.StagingDir(sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging, "pubspec.lock"), []byte("lock v1"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.PromoteStaging(sp.ID); err != nil {
		t.Fatal(err)
	}
	meta := &OutputMeta{Hash: "h1"}
	if err := s.WriteOutputMarker(sp.ID, meta); err != nil {
		t.Fatal(err)
	}
	if !s.OutputsValid(sp.ID, "h1") {
		t.Error("expected outputs valid for h1")
	}
	if s.OutputsValid(sp.ID, "h2") {
		t.Error("expected outputs invalid for unknown hash")
	}
	// Failed update path: staging for h2 exists but is never promoted.
	staging2, err := s.StagingDir(sp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staging2, "pubspec.lock"), []byte("lock v2 bad"), 0644); err != nil {
		t.Fatal(err)
	}
	// Simulate failure cleanup: remove staging, keep outputs.
	if err := os.RemoveAll(staging2); err != nil {
		t.Fatal(err)
	}
	if !s.OutputsValid(sp.ID, "h1") {
		t.Error("failed update must not destroy last good output")
	}
	out, _ := s.OutputsDir(sp.ID)
	kept, _ := os.ReadFile(filepath.Join(out, "pubspec.lock"))
	if string(kept) != "lock v1" {
		t.Errorf("expected last good output preserved, got %q", kept)
	}
}

func TestStoreRemoveScoped(t *testing.T) {
	s := testStore(t)
	a, b := testSpec("spec_del1"), testSpec("spec_keep1")
	for _, sp := range []*Spec{a, b} {
		if err := s.EnsureSpecDirs(sp.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.Save(sp); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Remove("spec_del1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Load("spec_del1"); err == nil {
		t.Error("expected deleted spec to be gone")
	}
	if _, err := s.Load("spec_keep1"); err != nil {
		t.Errorf("sibling spec must survive deletion: %v", err)
	}
	if err := s.Remove("../escape"); err == nil {
		t.Error("expected unsafe id to be rejected")
	}
	// Root itself must still exist with the sibling intact.
	if _, err := os.Stat(s.Root); err != nil {
		t.Errorf("store root must survive scoped deletion: %v", err)
	}
}
