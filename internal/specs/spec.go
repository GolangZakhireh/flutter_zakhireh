// Package specs implements the Parity-style file-based workflow on top of
// FlutterZakhireh: users upload a POP Spec YAML file, the backend validates
// and stores it, prepares an example project, resolves dependencies via the
// existing `flutter pub get` mechanism (through this proxy), and records the
// generation result.
//
// A Spec has a stable identity (ID) that is separate from its content
// versions (SHA-256 of the uploaded bytes) and its processing runs. See
// doc.go for the duplicate-upload and update policy.
package specs

import (
	"regexp"
	"time"
)

// Processing statuses exposed by the dashboard and API.
const (
	StatusUploaded   = "Uploaded"
	StatusQueued     = "Queued"
	StatusProcessing = "Processing"
	StatusCompleted  = "Completed"
	StatusFailed     = "Failed"
)

// validID restricts spec/run identifiers accepted from API requests so they
// can safely be used as directory names.
var validID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidID reports whether id is safe to use as a path segment and lookup key.
func ValidID(id string) bool { return validID.MatchString(id) }

// Spec is the persistent record for one managed specification.
type Spec struct {
	ID string `json:"id"`
	// OriginalFilename is the client-supplied name, kept as metadata only.
	// It is never used as a filesystem path.
	OriginalFilename string `json:"original_filename"`
	// Name is the logical spec name from the YAML (spec.name), if present.
	Name string `json:"name,omitempty"`
	// CurrentHash is the SHA-256 of the latest uploaded content.
	CurrentHash string `json:"current_hash"`
	// LastGoodHash is the SHA-256 of the last successfully processed content.
	// Empty when nothing has succeeded yet.
	LastGoodHash string `json:"last_good_hash,omitempty"`
	// Stale is true when the current content differs from the last
	// successfully processed content.
	Stale         bool       `json:"stale"`
	Status        string     `json:"status"`
	Error         string     `json:"error,omitempty"`
	UploadedAt    time.Time  `json:"uploaded_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
	LastAttemptAt *time.Time `json:"last_attempt_at,omitempty"`
	LastSuccessAt *time.Time `json:"last_success_at,omitempty"`
	// CurrentRunID is set while a job is Queued/Processing.
	CurrentRunID string `json:"current_run_id,omitempty"`
	// Output describes the last successfully promoted generation output.
	Output *OutputMeta `json:"output,omitempty"`
	// Runs holds recent processing attempts, newest last (bounded).
	Runs []Run `json:"runs,omitempty"`
	// Deleted marks a record pending removal; workers check it before promotion.
	Deleted bool `json:"-"`
}

// Run records a single processing attempt for a spec version.
type Run struct {
	ID        string     `json:"id"`
	SpecID    string     `json:"spec_id"`
	Hash      string     `json:"content_hash"`
	Status    string     `json:"status"`
	ExitCode  *int       `json:"exit_code,omitempty"`
	Error     string     `json:"error,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	LogFile   string     `json:"log_file,omitempty"`
}

// OutputMeta describes the last successfully generated output.
type OutputMeta struct {
	Hash        string    `json:"content_hash"`
	GeneratedAt time.Time `json:"generated_at"`
	RunID       string    `json:"run_id"`
	// Packages lists cached packages observed after generation
	// (name@version entries referencing the proxy cache).
	Packages []string `json:"packages,omitempty"`
	// Files lists files preserved under outputs/ (e.g. pubspec.lock).
	Files []string `json:"files,omitempty"`
	// ExampleProject is the workspace path (internal, not exposed verbatim;
	// handlers map it to a safe relative form).
	ExampleProject string `json:"example_project,omitempty"`
}

// PopSpec is the minimal project-specific POP Spec schema.
//
// NOTE: this is not an OpenAPI document. The repository contains no OpenAPI
// tooling and no pre-existing spec format, so POP Spec is defined here as a
// small pubspec-like subset: enough to create an example project and resolve
// dependencies with the existing `flutter pub get` workflow.
type PopSpec struct {
	Name            string         `yaml:"name"`
	Version         string         `yaml:"version,omitempty"`
	Description     string         `yaml:"description,omitempty"`
	Environment     map[string]any `yaml:"environment,omitempty"`
	Dependencies    map[string]any `yaml:"dependencies,omitempty"`
	DevDependencies map[string]any `yaml:"dev_dependencies,omitempty"`
	Flutter         map[string]any `yaml:"flutter,omitempty"`
}

// allStatuses lists every status a Spec or Run may carry.
func allStatuses() []string {
	return []string{StatusUploaded, StatusQueued, StatusProcessing, StatusCompleted, StatusFailed}
}

// isTerminal reports whether no further transitions are expected without a
// new upload or explicit retry.
func isTerminal(status string) bool {
	return status == StatusCompleted || status == StatusFailed || status == StatusUploaded
}
