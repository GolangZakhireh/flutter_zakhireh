package specs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGen is a controllable Generator for pipeline tests.
type fakeGen struct {
	mu    sync.Mutex
	calls int

	// block, when non-nil, pauses generation until closed (ctx-aware).
	block chan struct{}
	// fail makes Generate return a RunError.
	fail     bool
	exitCode int
}

func (f *fakeGen) Generate(ctx context.Context, req GenerateRequest) (GenerateResult, error) {
	f.mu.Lock()
	f.calls++
	block := f.block
	fail := f.fail
	code := f.exitCode
	f.mu.Unlock()

	if block != nil {
		select {
		case <-ctx.Done():
			return GenerateResult{}, ctx.Err()
		case <-block:
		}
	}
	if fail {
		if code == 0 {
			code = 3
		}
		fmt.Fprintf(req.Log, "fake generator failing exit=%d\n", code)
		return GenerateResult{}, &RunError{ExitCode: code, Msg: fmt.Sprintf("fake failure exit=%d", code)}
	}
	fmt.Fprintln(req.Log, "fake generator ok")
	if err := os.WriteFile(filepath.Join(req.Staging, "fake-output.txt"), []byte("ok"), 0644); err != nil {
		return GenerateResult{}, err
	}
	return GenerateResult{ExitCode: 0, Packages: []string{"http"}, Files: []string{"fake-output.txt"}}, nil
}

func (f *fakeGen) numCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeGen) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail = fail
}

func testService(t *testing.T, gen Generator) *Service {
	t.Helper()
	store := NewStore(t.TempDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	svc := NewService(store, Options{
		Generator: gen,
		ServerURL: "http://127.0.0.1:8811",
		Timeout:   30 * time.Second,
		MaxJobs:   2,
		MaxUpload: 1 << 20,
	})
	svc.Start()
	t.Cleanup(svc.Stop)
	return svc
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func waitStatus(svc *Service, id, status string) error {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		spec, err := svc.Get(id)
		if err == nil && spec.Status == status {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	spec, _ := svc.Get(id)
	got := "<missing>"
	if spec != nil {
		got = spec.Status
	}
	return fmt.Errorf("timed out waiting for %s (got %s)", status, got)
}

const specV1 = "name: demo_app\ndependencies:\n  http: ^1.1.0\n"
const specV2 = "name: demo_app\ndependencies:\n  http: ^1.2.0\n"

func TestUploadTriggersRealPipeline(t *testing.T) {
	gen := &fakeGen{}
	svc := testService(t, gen)
	spec, out, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Created || out.Deduped || out.Conflict {
		t.Errorf("expected fresh creation, got %+v", out)
	}
	if err := waitStatus(svc, spec.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(spec.ID)
	if got.LastSuccessAt == nil || got.LastAttemptAt == nil {
		t.Error("expected timestamps to be recorded")
	}
	if got.Output == nil || len(got.Output.Packages) != 1 {
		t.Errorf("expected output metadata, got %+v", got.Output)
	}
	if gen.numCalls() != 1 {
		t.Errorf("expected exactly 1 generation, got %d", gen.numCalls())
	}
	// Refresh/persistence: reload from disk sees the same state.
	reloaded, err := svc.store.Load(spec.ID)
	if err != nil || reloaded.Status != StatusCompleted {
		t.Errorf("state must survive a reload: %v %+v", err, reloaded)
	}
}

func TestUploadInvalidRejected(t *testing.T) {
	gen := &fakeGen{}
	svc := testService(t, gen)
	if _, _, err := svc.Upload("bad.json", []byte(specV1)); err == nil {
		t.Error("expected extension rejection")
	}
	if _, _, err := svc.Upload("bad.yaml", []byte("name: [broken")); err == nil {
		t.Error("expected YAML syntax rejection")
	}
	if _, _, err := svc.Upload("noname.yaml", []byte("dependencies:\n  http: x\n")); err == nil {
		t.Error("expected structure rejection")
	}
	if gen.numCalls() != 0 {
		t.Error("invalid uploads must never start jobs")
	}
}

func TestIdenticalUploadDeduped(t *testing.T) {
	gen := &fakeGen{}
	svc := testService(t, gen)
	first, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, first.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	second, out, err := svc.Upload("renamed.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if !out.Deduped {
		t.Errorf("expected deduped identical upload, got %+v", out)
	}
	if second.ID != first.ID {
		t.Error("identical content must map to the existing spec, not a new one")
	}
	if gen.numCalls() != 1 {
		t.Errorf("identical upload must not regenerate, calls=%d", gen.numCalls())
	}
}

func TestChangedSpecMarksStaleThenSucceeds(t *testing.T) {
	gen := &fakeGen{block: make(chan struct{})}
	svc := testService(t, gen)
	first, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	close(gen.block) // let v1 finish fast
	if err := waitStatus(svc, first.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	// Hold v2 in Processing to observe the stale flag.
	gen.mu.Lock()
	gen.block = make(chan struct{})
	gen.mu.Unlock()
	second, out, err := svc.Upload("demo.yaml", []byte(specV2))
	if err != nil {
		t.Fatal(err)
	}
	if out.Deduped || out.Created {
		t.Errorf("changed content must version the same spec, got %+v", out)
	}
	if second.ID != first.ID {
		t.Error("same logical name must keep the stable spec identity")
	}
	if err := waitStatus(svc, second.ID, StatusProcessing); err != nil {
		t.Fatal(err)
	}
	mid, _ := svc.Get(second.ID)
	if !mid.Stale {
		t.Error("spec must read stale while new content is unprocessed")
	}
	gen.mu.Lock()
	close(gen.block)
	gen.mu.Unlock()
	if err := waitStatus(svc, second.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	done, _ := svc.Get(second.ID)
	if done.Stale || done.LastGoodHash != Hash([]byte(specV2)) {
		t.Errorf("stale must clear after success: %+v", done)
	}
}

func TestFailedRunRetry(t *testing.T) {
	gen := &fakeGen{}
	gen.setFail(true)
	svc := testService(t, gen)
	spec, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, spec.ID, StatusFailed); err != nil {
		t.Fatal(err)
	}
	failed, _ := svc.Get(spec.ID)
	if failed.Error == "" || len(failed.Runs) == 0 {
		t.Error("failure details and run history must be preserved")
	}
	if _, err := svc.Log(spec.ID, ""); err != nil {
		t.Errorf("run log must be retrievable: %v", err)
	}
	gen.setFail(false)
	if _, err := svc.Retry(spec.ID); err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, spec.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
}

func TestFailureSurfacesLogTail(t *testing.T) {
	gen := &fakeGen{}
	gen.setFail(true)
	svc := testService(t, gen)
	spec, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, spec.ID, StatusFailed); err != nil {
		t.Fatal(err)
	}
	failed, _ := svc.Get(spec.ID)
	if !strings.Contains(failed.Error, "fake generator failing") {
		t.Errorf("dashboard error must contain the log cause, got %q", failed.Error)
	}
}

func TestConcurrentIdenticalUploadSingleFlight(t *testing.T) {
	block := make(chan struct{})
	gen := &fakeGen{block: block}
	svc := testService(t, gen)
	first, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, first.ID, StatusProcessing); err != nil {
		t.Fatal(err)
	}
	_, out, err := svc.Upload("demo-copy.yaml", []byte(specV1))
	if !errors.Is(err, ErrConflict) || !out.Conflict {
		t.Errorf("duplicate in-progress version must conflict, got out=%+v err=%v", out, err)
	}
	close(block)
	if err := waitStatus(svc, first.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	if gen.numCalls() != 1 {
		t.Errorf("concurrent duplicates must not launch extra jobs, calls=%d", gen.numCalls())
	}
}

func TestFailedUpdatePreservesLastGood(t *testing.T) {
	gen := &fakeGen{}
	svc := testService(t, gen)
	spec, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, spec.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	gen.setFail(true)
	updated, _, err := svc.Upload("demo.yaml", []byte(specV2))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, updated.ID, StatusFailed); err != nil {
		t.Fatal(err)
	}
	meta, _, err := svc.Outputs(updated.ID)
	if err != nil {
		t.Fatalf("last good output must survive a failed update: %v", err)
	}
	if meta.Hash != Hash([]byte(specV1)) {
		t.Errorf("output marker must still reference v1, got %s", meta.Hash)
	}
}

func TestDeleteCleansOnlyOwnResources(t *testing.T) {
	gen := &fakeGen{}
	svc := testService(t, gen)
	a, _, err := svc.Upload("a.yaml", []byte("name: app_a\ndependencies:\n  http: ^1.0.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := svc.Upload("b.yaml", []byte("name: app_b\ndependencies:\n  http: ^1.0.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, a.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, b.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := svc.Delete(a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Error("deleted spec must be gone")
	}
	if _, err := svc.Get(b.ID); err != nil {
		t.Errorf("sibling spec must survive: %v", err)
	}
	if _, _, err := svc.Outputs(b.ID); err != nil {
		t.Errorf("sibling outputs must survive: %v", err)
	}
}

func TestDeleteWhileProcessing(t *testing.T) {
	block := make(chan struct{})
	gen := &fakeGen{block: block}
	svc := testService(t, gen)
	other, _, err := svc.Upload("other.yaml", []byte("name: other_app\ndependencies:\n  http: ^1.0.0\n"))
	if err != nil {
		t.Fatal(err)
	}
	victim, _, err := svc.Upload("demo.yaml", []byte(specV1))
	if err != nil {
		t.Fatal(err)
	}
	// Wait until at least one job is processing, then delete the victim.
	waitFor(t, "a job processing", func() bool {
		a, _ := svc.Get(other.ID)
		v, _ := svc.Get(victim.ID)
		return (a != nil && a.Status == StatusProcessing) || (v != nil && v.Status == StatusProcessing)
	})
	done := make(chan error, 1)
	go func() { done <- svc.Delete(victim.ID) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("delete during processing failed: %v", err)
		}
	case <-time.After(35 * time.Second):
		t.Fatal("delete blocked while job running")
	}
	close(block) // release the surviving job, if any
	time.Sleep(300 * time.Millisecond)
	if _, err := svc.Get(victim.ID); !errors.Is(err, ErrNotFound) {
		t.Error("deleted spec must stay deleted after its job observes cancellation")
	}
	// The store must not resurrect metadata for the victim.
	if _, err := svc.store.Load(victim.ID); err == nil {
		t.Error("worker must not recreate the deleted record")
	}
	// Other specs keep working.
	if err := waitStatus(svc, other.ID, StatusCompleted); err != nil {
		t.Errorf("sibling job must be unaffected: %v", err)
	}
}

func TestRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash: a record stuck in Processing with no live worker.
	id := "spec_crash1"
	now := time.Now().UTC()
	stuck := &Spec{
		ID:               id,
		OriginalFilename: "crash.yaml",
		Name:             "crash_app",
		CurrentHash:      Hash([]byte(specV1)),
		Status:           StatusProcessing,
		CurrentRunID:     "run_orphan",
		UploadedAt:       now,
		UpdatedAt:        now,
		Runs:             []Run{{ID: "run_orphan", SpecID: id, Status: StatusProcessing, StartedAt: now}},
	}
	if err := store.EnsureSpecDirs(id); err != nil {
		t.Fatal(err)
	}
	if err := store.WriteVersion(id, stuck.CurrentHash, []byte(specV1)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(stuck); err != nil {
		t.Fatal(err)
	}

	gen := &fakeGen{}
	svc := NewService(store, Options{Generator: gen, Timeout: 30 * time.Second, MaxJobs: 1})
	svc.Start()
	t.Cleanup(svc.Stop)
	if err := waitStatus(svc, id, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.Get(id)
	if len(got.Runs) == 0 || got.Runs[0].Status != StatusFailed {
		t.Errorf("interrupted run must be marked failed, got %+v", got.Runs)
	}
}

func TestTwoSpecsIsolated(t *testing.T) {
	gen := &fakeGen{}
	svc := testService(t, gen)
	a, _, _ := svc.Upload("a.yaml", []byte("name: app_a\ndependencies:\n  http: ^1.0.0\n"))
	b, _, _ := svc.Upload("b.yaml", []byte("name: app_b\ndependencies:\n  provider: ^6.0.0\n"))
	if a.ID == b.ID {
		t.Fatal("different specs need different identities")
	}
	if err := waitStatus(svc, a.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	if err := waitStatus(svc, b.ID, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	ma, _, _ := svc.Outputs(a.ID)
	mb, _, _ := svc.Outputs(b.ID)
	if ma.Hash == mb.Hash {
		t.Error("outputs must be isolated per spec")
	}
}
