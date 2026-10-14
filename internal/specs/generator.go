package specs

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// GenerateRequest is the integration boundary between the dashboard and the
// actual package-generation capability: an example project derived from the
// uploaded spec, dependency resolution, and output capture.
type GenerateRequest struct {
	SpecID    string
	Parsed    *PopSpec
	Workspace string // example-project directory (durable, per spec)
	Staging   string // fresh staging directory for this run's outputs
	Log       io.Writer
	// ServerURL is this proxy's base URL; it becomes PUB_HOSTED_URL so
	// resolution flows through the existing proxy cache (the same mechanism
	// scripts/prime_cache.sh relies on).
	ServerURL string
	// Bin overrides the flutter/dart binary lookup.
	Bin string
}

// GenerateResult summarizes a successful run.
type GenerateResult struct {
	ExitCode int
	Packages []string
	Files    []string
}

// Generator runs package generation for one spec version. Implementations
// must never execute uploaded YAML as code; the spec only supplies data
// (dependency declarations) for a fixed, safe command line.
type Generator interface {
	Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error)
}

// FlutterPubGenerator is the real integration: it materializes the example
// project from the spec and runs `<flutter|dart> pub get` with
// PUB_HOSTED_URL pointed at this proxy, reusing the exact workflow from
// scripts/prime_cache.sh and the proxy cache.
type FlutterPubGenerator struct{}

// Generate implements Generator.
func (g *FlutterPubGenerator) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	if req.Parsed == nil {
		return GenerateResult{}, fmt.Errorf("no parsed spec")
	}
	if req.Workspace == "" || req.Staging == "" {
		return GenerateResult{}, fmt.Errorf("workspace and staging directories are required")
	}
	bin, err := resolveBin(req.Bin)
	if err != nil {
		return GenerateResult{}, err
	}
	if err := writeExamplePubspec(req.Workspace, req.Parsed); err != nil {
		return GenerateResult{}, fmt.Errorf("prepare example project: %w", err)
	}

	// Fixed argument list — no shell, no interpolation of spec content into
	// commands. Spec data only ever lives in files.
	cmd := exec.CommandContext(ctx, bin, "pub", "get")
	cmd.Dir = req.Workspace
	cmd.Env = pubEnv(req.ServerURL)
	if req.Log != nil {
		cmd.Stdout = req.Log
		cmd.Stderr = req.Log
	}
	fmt.Fprintf(req.Log, "--- parity generate: %s pub get (workspace %s) ---\n", bin, req.Workspace)
	start := time.Now()
	runErr := cmd.Run()
	elapsed := time.Since(start)
	exitCode := 0
	if runErr != nil {
		if ee, ok := runErr.(*exec.ExitError); ok {
			exitCode = ee.ExitCode()
		} else {
			return GenerateResult{}, fmt.Errorf("run %s pub get: %w", bin, runErr)
		}
	}
	fmt.Fprintf(req.Log, "--- pub get finished exit=%d in %s ---\n", exitCode, elapsed)
	if exitCode != 0 {
		return GenerateResult{}, &RunError{ExitCode: exitCode, Msg: fmt.Sprintf("%s pub get failed with exit code %d (see run log)", bin, exitCode)}
	}

	packages := readLockPackages(filepath.Join(req.Workspace, "pubspec.lock"))
	files, err := snapshotOutputs(req.Workspace, req.Staging, req.SpecID, packages, exitCode)
	if err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{ExitCode: 0, Packages: packages, Files: files}, nil
}

// RunError carries a process exit code for failed generations.
type RunError struct {
	ExitCode int
	Msg      string
}

func (e *RunError) Error() string { return e.Msg }

// resolveBin picks the dependency-resolution binary: explicit override,
// then PATH lookup (flutter, then dart), then well-known mount points used
// by the Docker setup (/opt/flutter, /opt/dart-sdk, /usr/lib/dart). A clear
// error (not a fake success) is returned when no SDK is installed.
func resolveBin(override string) (string, error) {
	if override != "" {
		if _, err := os.Stat(override); err == nil {
			return override, nil
		}
		if p, err := exec.LookPath(override); err == nil {
			return p, nil
		}
		return "", fmt.Errorf("configured generator binary %q not found: install the Flutter/Dart SDK or set FLUTTERZAKHIREH_FLUTTER_BIN", override)
	}
	for _, name := range []string{"flutter", "dart"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, nil
		}
	}
	for _, p := range []string{
		"/opt/flutter/bin/flutter",
		"/opt/dart-sdk/bin/dart",
		"/usr/lib/dart/bin/dart",
	} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	return "", fmt.Errorf("neither 'flutter' nor 'dart' found in PATH: install the Flutter/Dart SDK or set FLUTTERZAKHIREH_FLUTTER_BIN")
}

// pubEnv builds the child environment: inherited env plus PUB_HOSTED_URL
// pointed at this proxy so packages resolve (and cache) through it.
func pubEnv(serverURL string) []string {
	env := os.Environ()
	if serverURL == "" {
		return env
	}
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PUB_HOSTED_URL=") {
			out = append(out, kv)
		}
	}
	return append(out, "PUB_HOSTED_URL="+serverURL)
}

// writeExamplePubspec materializes the example project from the spec. This
// mirrors the pubspec generation in scripts/prime_cache.sh.
func writeExamplePubspec(workspace string, spec *PopSpec) error {
	doc := map[string]any{
		"name":        fmt.Sprintf("%s_example", spec.Name),
		"description": fmt.Sprintf("Example project generated by Parity for spec %q.", spec.Name),
		"publish_to":  "none",
		"version":     firstNonEmpty(spec.Version, "0.1.0"),
		"environment": map[string]any{"sdk": ">=3.0.0 <4.0.0"},
	}
	if spec.Environment != nil {
		doc["environment"] = spec.Environment
	}
	if spec.Dependencies != nil {
		doc["dependencies"] = spec.Dependencies
	}
	if spec.DevDependencies != nil {
		doc["dev_dependencies"] = spec.DevDependencies
	} else {
		doc["dev_dependencies"] = map[string]any{"flutter_lints": "^3.0.0"}
	}
	if spec.Flutter != nil {
		doc["flutter"] = spec.Flutter
	}
	data, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(workspace, 0755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(workspace, "pubspec.yaml"), data, 0644)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// readLockPackages extracts resolved package names from pubspec.lock with a
// small line-oriented scan (avoids depending on lockfile schema details).
func readLockPackages(lockPath string) []string {
	f, err := os.Open(lockPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var pkgs []string
	inPackages := false
	seen := map[string]bool{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if line == "packages:" {
			inPackages = true
			continue
		}
		if inPackages {
			if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "    ") && strings.HasSuffix(line, ":") {
				name := strings.TrimSpace(strings.TrimSuffix(line, ":"))
				if name != "" && name != "packages" && !seen[name] {
					seen[name] = true
					pkgs = append(pkgs, name)
				}
				continue
			}
			if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") {
				break
			}
		}
	}
	sort.Strings(pkgs)
	return pkgs
}

// snapshotOutputs copies durable artifacts into staging and writes a
// generation summary. Only an allowlist of files is copied; the workspace
// may contain tool-created files that must not leak into outputs.
func snapshotOutputs(workspace, staging, specID string, packages []string, exitCode int) ([]string, error) {
	if err := os.MkdirAll(staging, 0755); err != nil {
		return nil, err
	}
	var files []string
	for _, name := range []string{"pubspec.yaml", "pubspec.lock"} {
		src := filepath.Join(workspace, name)
		if st, err := os.Stat(src); err != nil || st.IsDir() {
			continue
		}
		data, err := os.ReadFile(src)
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(staging, name), data, 0644); err != nil {
			return nil, err
		}
		files = append(files, name)
	}
	summary := map[string]any{
		"spec_id":      specID,
		"exit_code":    exitCode,
		"packages":     packages,
		"files":        files,
		"generated_at": time.Now().UTC().Format(time.RFC3339),
	}
	sdata, _ := json.MarshalIndent(summary, "", "  ")
	if err := os.WriteFile(filepath.Join(staging, "generation.json"), sdata, 0644); err != nil {
		return nil, err
	}
	files = append(files, "generation.json")
	sort.Strings(files)
	return files, nil
}
