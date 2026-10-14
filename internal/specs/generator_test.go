package specs

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeStub creates an executable shell stub used as the generator binary.
// The real generator invokes `<bin> pub get` with Dir=workspace, so a stub
// that mimics pub's file effects exercises the full integration boundary
// (example-project materialization, subprocess capture, output snapshot)
// without needing the Flutter SDK.
func writeStub(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "stub-pub")
	if err := os.WriteFile(p, []byte(body), 0755); err != nil {
		t.Fatal(err)
	}
	return p
}

const successStub = `#!/bin/sh
echo "stub pub $* in $(pwd)"
cat > pubspec.lock <<'EOF'
packages:
  http:
    dependency: "direct main"
    version: "1.1.0"
  meta:
    dependency: transitive
    version: "1.9.1"
sdks:
  dart: ">=3.0.0 <4.0.0"
EOF
exit 0
`

const failStub = `#!/bin/sh
echo "stub failure: resolution impossible" >&2
exit 3
`

func TestFlutterPubGeneratorSuccess(t *testing.T) {
	bin := writeStub(t, successStub)
	gen := &FlutterPubGenerator{}
	ws := t.TempDir()
	staging := t.TempDir()
	var log bytes.Buffer
	parsed, verr := ParseAndValidate([]byte("name: stub_app\ndependencies:\n  http: ^1.1.0\n"))
	if verr != nil {
		t.Fatal(verr)
	}
	res, err := gen.Generate(context.Background(), GenerateRequest{
		SpecID:    "spec_stub",
		Parsed:    parsed,
		Workspace: ws,
		Staging:   staging,
		Log:       &log,
		ServerURL: "http://127.0.0.1:8811",
		Bin:       bin,
	})
	if err != nil {
		t.Fatalf("expected success, got %v (log: %s)", err, log.String())
	}
	if res.ExitCode != 0 {
		t.Errorf("expected exit 0, got %d", res.ExitCode)
	}
	if len(res.Packages) != 2 || res.Packages[0] != "http" || res.Packages[1] != "meta" {
		t.Errorf("expected [http meta], got %v", res.Packages)
	}
	// Example project materialized from the spec.
	pubspec, err := os.ReadFile(filepath.Join(ws, "pubspec.yaml"))
	if err != nil || !strings.Contains(string(pubspec), "stub_app_example") {
		t.Errorf("expected generated example pubspec, got %v %q", err, pubspec)
	}
	// Staging snapshot allowlist.
	for _, name := range []string{"pubspec.yaml", "pubspec.lock", "generation.json"} {
		if _, err := os.Stat(filepath.Join(staging, name)); err != nil {
			t.Errorf("expected %s in staging: %v", name, err)
		}
	}
	if !strings.Contains(log.String(), "stub pub ") {
		t.Errorf("expected subprocess output captured, got %q", log.String())
	}
}

func TestFlutterPubGeneratorFailure(t *testing.T) {
	bin := writeStub(t, failStub)
	gen := &FlutterPubGenerator{}
	ws := t.TempDir()
	var log bytes.Buffer
	parsed, _ := ParseAndValidate([]byte("name: stub_app\ndependencies:\n  http: ^1.1.0\n"))
	_, err := gen.Generate(context.Background(), GenerateRequest{
		SpecID: "spec_fail", Parsed: parsed, Workspace: ws,
		Staging: t.TempDir(), Log: &log, Bin: bin,
	})
	if err == nil {
		t.Fatal("expected failure")
	}
	re, ok := err.(*RunError)
	if !ok {
		t.Fatalf("expected *RunError, got %T", err)
	}
	if re.ExitCode != 3 {
		t.Errorf("expected exit code 3, got %d", re.ExitCode)
	}
	if !strings.Contains(log.String(), "stub failure") {
		t.Errorf("expected stderr captured, got %q", log.String())
	}
}

func TestFlutterPubGeneratorMissingBinary(t *testing.T) {
	gen := &FlutterPubGenerator{}
	var log bytes.Buffer
	parsed, _ := ParseAndValidate([]byte("name: stub_app\ndependencies:\n  http: ^1.1.0\n"))
	_, err := gen.Generate(context.Background(), GenerateRequest{
		SpecID: "spec_nobin", Parsed: parsed, Workspace: t.TempDir(),
		Staging: t.TempDir(), Log: &log, Bin: "/nonexistent/flutter-bin",
	})
	if err == nil || !strings.Contains(err.Error(), "FLUTTERZAKHIREH_FLUTTER_BIN") {
		t.Errorf("expected actionable missing-SDK error, got %v", err)
	}
}

func TestReadLockPackages(t *testing.T) {
	dir := t.TempDir()
	lock := "packages:\n  http:\n    version: \"1.1.0\"\n  meta:\n    version: \"1.9.1\"\nsdks:\n  dart: \">=3.0.0\"\n"
	if err := os.WriteFile(filepath.Join(dir, "pubspec.lock"), []byte(lock), 0644); err != nil {
		t.Fatal(err)
	}
	pkgs := readLockPackages(filepath.Join(dir, "pubspec.lock"))
	if len(pkgs) != 2 || pkgs[0] != "http" || pkgs[1] != "meta" {
		t.Errorf("unexpected packages %v", pkgs)
	}
	if readLockPackages(filepath.Join(dir, "missing.lock")) != nil {
		t.Error("missing lockfile must yield no packages, not an error")
	}
}
