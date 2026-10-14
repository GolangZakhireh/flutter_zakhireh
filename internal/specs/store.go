package specs

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// On-disk layout for one spec rooted at Store.Root:
//
//	<root>/<id>/source.yaml          latest uploaded bytes
//	<root>/<id>/metadata.json        Spec record
//	<root>/<id>/versions/<sha>.yaml  immutable version history
//	<root>/<id>/workspace/           generated example project
//	<root>/<id>/outputs/             last successfully promoted output
//	<root>/<id>/outputs_staging/     in-progress generation target
//	<root>/<id>/logs/<run-id>.log    captured process output
const (
	fileSource   = "source.yaml"
	fileMetadata = "metadata.json"
	dirVersions  = "versions"
	dirWorkspace = "workspace"
	dirOutputs   = "outputs"
	dirStaging   = "outputs_staging"
	dirLogs      = "logs"
	fileOutput   = "output.json"
)

// Store persists specs on the local filesystem. It performs no locking
// itself; callers (Service) serialize per-spec mutation.
type Store struct {
	Root string
}

// NewStore creates a store rooted at dir.
func NewStore(dir string) *Store { return &Store{Root: dir} }

// Init ensures the root directory exists.
func (s *Store) Init() error {
	return os.MkdirAll(s.Root, 0755)
}

// specDir resolves and contains id inside the store root.
func (s *Store) specDir(id string) (string, error) {
	if !ValidID(id) {
		return "", fmt.Errorf("invalid spec id")
	}
	dir := filepath.Join(s.Root, id)
	if err := s.contained(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// contained guards against path traversal outside the root.
func (s *Store) contained(path string) error {
	rel, err := filepath.Rel(s.Root, path)
	if err != nil {
		return fmt.Errorf("unsafe path")
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes store root")
	}
	return nil
}

// EnsureSpecDirs creates the managed subdirectories for a spec.
func (s *Store) EnsureSpecDirs(id string) error {
	dir, err := s.specDir(id)
	if err != nil {
		return err
	}
	for _, sub := range []string{"", dirVersions, dirWorkspace, dirOutputs, dirLogs} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0755); err != nil {
			return err
		}
	}
	return nil
}

// Save writes the spec record atomically.
func (s *Store) Save(spec *Spec) error {
	dir, err := s.specDir(spec.ID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, fileMetadata), data, 0644)
}

// Load reads a spec record.
func (s *Store) Load(id string) (*Spec, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, fileMetadata))
	if err != nil {
		return nil, err
	}
	var spec Spec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, fmt.Errorf("corrupt metadata for %q: %w", id, err)
	}
	return &spec, nil
}

// List returns all specs ordered by upload time (oldest first).
func (s *Store) List() ([]*Spec, error) {
	entries, err := os.ReadDir(s.Root)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []*Spec
	for _, e := range entries {
		if !e.IsDir() || !ValidID(e.Name()) {
			continue
		}
		spec, err := s.Load(e.Name())
		if err != nil {
			continue // skip corrupt/foreign entries, keep list usable
		}
		out = append(out, spec)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UploadedAt.Before(out[j].UploadedAt)
	})
	return out, nil
}

// Remove deletes the entire managed directory for a spec. It refuses paths
// outside the store root and never touches sibling specs.
func (s *Store) Remove(id string) error {
	dir, err := s.specDir(id)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

// WriteVersion stores immutable version bytes and refreshes source.yaml.
func (s *Store) WriteVersion(id, hash string, data []byte) error {
	dir, err := s.specDir(id)
	if err != nil {
		return err
	}
	if !ValidID(strings.ReplaceAll(hash, "-", "")) || strings.Contains(hash, "/") {
		return fmt.Errorf("invalid content hash")
	}
	if err := os.MkdirAll(filepath.Join(dir, dirVersions), 0755); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(dir, dirVersions, hash+".yaml"), data, 0644); err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, fileSource), data, 0644)
}

// ReadSource returns the latest uploaded bytes.
func (s *Store) ReadSource(id string) ([]byte, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(filepath.Join(dir, fileSource))
}

// ReadVersion returns the bytes of a historical version.
func (s *Store) ReadVersion(id, hash string) ([]byte, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return nil, err
	}
	p := filepath.Join(dir, dirVersions, hash+".yaml")
	if err := s.contained(p); err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// WorkspaceDir returns the example-project directory (created on demand).
func (s *Store) WorkspaceDir(id string) (string, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, dirWorkspace)
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}

// OutputsDir returns the promoted output directory (created on demand).
func (s *Store) OutputsDir(id string) (string, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, dirOutputs)
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}

// StagingDir returns a fresh staging directory for the next run: any
// leftover from a crashed run is removed first so stale files can never leak
// into a promotion.
func (s *Store) StagingDir(id string) (string, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, dirStaging)
	if err := s.contained(p); err != nil {
		return "", err
	}
	if err := os.RemoveAll(p); err != nil {
		return "", err
	}
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}

// PromoteStaging atomically replaces outputs/ with the staging result.
func (s *Store) PromoteStaging(id string) error {
	dir, err := s.specDir(id)
	if err != nil {
		return err
	}
	staging := filepath.Join(dir, dirStaging)
	outputs := filepath.Join(dir, dirOutputs)
	backup := filepath.Join(dir, dirOutputs+".bak")
	// Staging must exist; otherwise there is nothing to promote.
	if st, err := os.Stat(staging); err != nil || !st.IsDir() {
		return fmt.Errorf("no staging output to promote")
	}
	_ = os.RemoveAll(backup)
	if _, err := os.Stat(outputs); err == nil {
		if err := os.Rename(outputs, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, outputs); err != nil {
		// Best-effort rollback to the previous good output.
		if _, berr := os.Stat(backup); berr == nil {
			_ = os.Rename(backup, outputs)
		}
		return err
	}
	_ = os.RemoveAll(backup)
	return nil
}

// WriteOutputMarker records which content hash produced outputs/.
func (s *Store) WriteOutputMarker(id string, meta *OutputMeta) error {
	dir, err := s.specDir(id)
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(dir, dirOutputs, fileOutput), data, 0644)
}

// ReadOutputMarker returns the marker for outputs/, if present.
func (s *Store) ReadOutputMarker(id string) (*OutputMeta, error) {
	dir, err := s.specDir(id)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, dirOutputs, fileOutput))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta OutputMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return &meta, nil
}

// OutputsValid reports whether outputs/ holds a successful result for hash.
func (s *Store) OutputsValid(id, hash string) bool {
	meta, err := s.ReadOutputMarker(id)
	if err != nil || meta == nil {
		return false
	}
	if meta.Hash != hash {
		return false
	}
	dir, err := s.specDir(id)
	if err != nil {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, dirOutputs))
	return err == nil && st.IsDir()
}

// LogPath returns the path for a run log (parent dir created on demand).
func (s *Store) LogPath(id, runID string) (string, error) {
	if !ValidID(runID) {
		return "", fmt.Errorf("invalid run id")
	}
	dir, err := s.specDir(id)
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, dirLogs, runID+".log")
	if err := s.contained(p); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(dir, dirLogs), 0755); err != nil {
		return "", err
	}
	return p, nil
}

// ReadLog returns a run log.
func (s *Store) ReadLog(id, runID string) ([]byte, error) {
	p, err := s.LogPath(id, runID)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// atomicWrite writes via temp file + rename so crashes never leave
// half-written metadata.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return os.Rename(tmpName, path)
}
