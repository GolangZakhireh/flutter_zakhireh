package specs

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Sentinel errors for API mapping.
var (
	ErrNotFound = errors.New("spec not found")
	ErrConflict = errors.New("job already in progress for this spec version")
	ErrTooLarge = errors.New("upload exceeds size limit")
)

// UploadOutcome describes how an upload was matched to existing records.
type UploadOutcome struct {
	// Deduped is true when identical content was already processed
	// successfully and no new job was scheduled.
	Deduped bool
	// Conflict is true when the same version is already Queued/Processing.
	Conflict bool
	// Created is true when a brand-new Spec ID was registered.
	Created bool
}

// Options configures the Service.
type Options struct {
	Generator  Generator
	ServerURL  string
	Timeout    time.Duration
	MaxJobs    int
	MaxUpload  int64
	QueueDepth int
}

// Service orchestrates spec uploads, background generation jobs and deletion.
type Service struct {
	store     *Store
	gen       Generator
	serverURL string
	timeout   time.Duration
	maxUpload int64

	queue   chan string
	workers int

	mu      sync.Mutex
	locks   map[string]*sync.Mutex
	queued  map[string]bool
	running map[string]bool
	cancel  map[string]context.CancelFunc
	deleted map[string]bool

	wg     sync.WaitGroup
	closed chan struct{}
}

// NewService builds a service. Call Start to launch workers and recover state.
func NewService(store *Store, opts Options) *Service {
	maxJobs := opts.MaxJobs
	if maxJobs <= 0 {
		maxJobs = 2
	}
	depth := opts.QueueDepth
	if depth <= 0 {
		depth = 128
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 10 * time.Minute
	}
	maxUpload := opts.MaxUpload
	if maxUpload <= 0 {
		maxUpload = 1 << 20
	}
	return &Service{
		store:     store,
		gen:       opts.Generator,
		serverURL: opts.ServerURL,
		timeout:   timeout,
		maxUpload: maxUpload,
		queue:     make(chan string, depth),
		workers:   maxJobs,
		locks:     map[string]*sync.Mutex{},
		queued:    map[string]bool{},
		running:   map[string]bool{},
		cancel:    map[string]context.CancelFunc{},
		deleted:   map[string]bool{},
		closed:    make(chan struct{}),
	}
}

// Start launches workers and recovers interrupted jobs.
func (s *Service) Start() {
	s.Recover()
	for i := 0; i < s.workers; i++ {
		s.wg.Add(1)
		go s.worker()
	}
}

// Stop drains the queue and waits for workers. Running jobs are cancelled
// via context; their records are left in a recoverable Queued state.
func (s *Service) Stop() {
	select {
	case <-s.closed:
		return
	default:
		close(s.closed)
	}
	s.mu.Lock()
	for id, cancel := range s.cancel {
		cancel()
		delete(s.cancel, id)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

// specLock returns the per-spec mutex, creating it on demand.
func (s *Service) specLock(id string) *sync.Mutex {
	s.mu.Lock()
	defer s.mu.Unlock()
	lk, ok := s.locks[id]
	if !ok {
		lk = &sync.Mutex{}
		s.locks[id] = lk
	}
	return lk
}

// Hash returns the SHA-256 hex of uploaded bytes.
func Hash(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func newID(prefix string) string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().Unix(), hex.EncodeToString(b[:]))
}

// Upload validates, stores and schedules a spec file.
func (s *Service) Upload(filename string, data []byte) (*Spec, UploadOutcome, error) {
	var out UploadOutcome
	if verr := CheckExtension(filename); verr != nil {
		return nil, out, verr
	}
	if int64(len(data)) > s.maxUpload {
		return nil, out, &ValidationError{Field: "file", Message: fmt.Sprintf("file too large (%d bytes, limit %d)", len(data), s.maxUpload)}
	}
	parsed, verr := ParseAndValidate(data)
	if verr != nil {
		return nil, out, verr
	}
	hash := Hash(data)
	now := time.Now().UTC()

	existing, err := s.store.List()
	if err != nil {
		return nil, out, err
	}
	// 1-3: identical content match.
	for _, spec := range existing {
		if spec.CurrentHash != hash {
			continue
		}
		lk := s.specLock(spec.ID)
		lk.Lock()
		fresh, lerr := s.store.Load(spec.ID)
		if lerr != nil {
			lk.Unlock()
			continue
		}
		switch {
		case fresh.Status == StatusCompleted && s.store.OutputsValid(fresh.ID, hash):
			lk.Unlock()
			out.Deduped = true
			return fresh, out, nil
		case fresh.Status == StatusQueued || fresh.Status == StatusProcessing || s.isScheduled(fresh.ID):
			lk.Unlock()
			out.Conflict = true
			return fresh, out, ErrConflict
		default:
			// Failed/Uploaded identical content: reuse the record and
			// schedule a (re)run instead of duplicating it.
			// NOTE: lk is held here; only enqueue (never schedule) is
			// called to avoid re-locking the same spec mutex.
			fresh.OriginalFilename = SanitizeFilename(filename)
			fresh.UpdatedAt = now
			fresh.Error = ""
			prevStatus := fresh.Status
			fresh.Status = StatusQueued
			if err := s.store.Save(fresh); err != nil {
				lk.Unlock()
				return nil, out, err
			}
			if err := s.enqueue(fresh.ID); err != nil {
				fresh.Status = prevStatus
				_ = s.store.Save(fresh)
				lk.Unlock()
				return fresh, out, err
			}
			lk.Unlock()
			return fresh, out, nil
		}
	}
	// 4: same logical name, new content -> new version under same spec.
	for _, spec := range existing {
		if parsed.Name == "" || spec.Name != parsed.Name {
			continue
		}
		lk := s.specLock(spec.ID)
		lk.Lock()
		fresh, lerr := s.store.Load(spec.ID)
		if lerr != nil {
			lk.Unlock()
			continue
		}
		if fresh.CurrentHash == hash {
			lk.Unlock()
			continue // raced with identical match above; skip
		}
		if err := s.store.WriteVersion(fresh.ID, hash, data); err != nil {
			lk.Unlock()
			return nil, out, err
		}
		fresh.OriginalFilename = SanitizeFilename(filename)
		fresh.CurrentHash = hash
		fresh.Stale = fresh.LastGoodHash != "" && fresh.LastGoodHash != hash
		fresh.UpdatedAt = now
		fresh.Error = ""
		prevStatus := fresh.Status
		fresh.Status = StatusQueued
		if err := s.store.Save(fresh); err != nil {
			lk.Unlock()
			return nil, out, err
		}
		// lk is held: enqueue only (see note above).
		if err := s.enqueue(fresh.ID); err != nil {
			fresh.Status = prevStatus
			_ = s.store.Save(fresh)
			lk.Unlock()
			return fresh, out, err
		}
		lk.Unlock()
		return fresh, out, nil
	}
	// 5: brand-new spec.
	id := newID("spec")
	spec := &Spec{
		ID:               id,
		OriginalFilename: SanitizeFilename(filename),
		Name:             parsed.Name,
		CurrentHash:      hash,
		Status:           StatusUploaded,
		UploadedAt:       now,
		UpdatedAt:        now,
	}
	if err := s.store.EnsureSpecDirs(id); err != nil {
		return nil, out, err
	}
	if err := s.store.WriteVersion(id, hash, data); err != nil {
		return nil, out, err
	}
	if err := s.store.Save(spec); err != nil {
		return nil, out, err
	}
	out.Created = true
	// New ID: no lock held. Persist Queued, then enqueue.
	spec.Status = StatusQueued
	if err := s.store.Save(spec); err != nil {
		return spec, out, err
	}
	if err := s.enqueue(id); err != nil {
		spec.Status = StatusUploaded
		_ = s.store.Save(spec)
		return spec, out, err
	}
	return spec, out, nil
}

// Retry schedules a new run for the current content. Safe and idempotent:
// terminal states start exactly one run; in-progress versions conflict.
func (s *Service) Retry(id string) (*Spec, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	lk := s.specLock(id)
	lk.Lock()
	defer lk.Unlock()
	spec, err := s.store.Load(id)
	if err != nil {
		return nil, ErrNotFound
	}
	if s.isScheduled(id) || spec.Status == StatusQueued || spec.Status == StatusProcessing {
		return spec, ErrConflict
	}
	spec.Error = ""
	spec.UpdatedAt = time.Now().UTC()
	prevStatus := spec.Status
	spec.Status = StatusQueued
	if err := s.store.Save(spec); err != nil {
		return nil, err
	}
	// lk is held: enqueue only, roll back to the previous idle state on
	// failure so the record never strands in Queued without a queued job.
	if err := s.enqueue(id); err != nil {
		spec.Status = prevStatus
		_ = s.store.Save(spec)
		return spec, err
	}
	return spec, nil
}

// Delete cancels any active job and removes the spec's managed files and
// record. Shared or foreign files are never touched: removal is confined to
// the spec's own directory. A tombstone is kept so a concurrently finishing
// worker cannot resurrect the record or promote outputs.
func (s *Service) Delete(id string) error {
	if !ValidID(id) {
		return ErrNotFound
	}
	lk := s.specLock(id)
	lk.Lock()
	if _, err := s.store.Load(id); err != nil {
		lk.Unlock()
		return ErrNotFound
	}
	s.mu.Lock()
	s.deleted[id] = true
	if cancel, ok := s.cancel[id]; ok {
		cancel()
	}
	s.mu.Unlock()
	lk.Unlock()

	// Wait for a running job to observe cancellation (bounded grace).
	deadline := time.Now().Add(30 * time.Second)
	for {
		s.mu.Lock()
		stillRunning := s.running[id]
		s.mu.Unlock()
		if !stillRunning {
			break
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	lk.Lock()
	defer lk.Unlock()
	// Re-check: a concurrent delete may have finished first.
	if _, err := s.store.Load(id); err != nil {
		s.mu.Lock()
		delete(s.queued, id)
		delete(s.running, id)
		delete(s.cancel, id)
		s.mu.Unlock()
		return nil
	}
	if err := s.store.Remove(id); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.queued, id)
	delete(s.running, id)
	delete(s.cancel, id)
	s.mu.Unlock()
	return nil
}

// Get loads one spec.
func (s *Service) Get(id string) (*Spec, error) {
	if !ValidID(id) {
		return nil, ErrNotFound
	}
	spec, err := s.store.Load(id)
	if err != nil {
		return nil, ErrNotFound
	}
	return spec, nil
}

// List returns all specs.
func (s *Service) List() ([]*Spec, error) { return s.store.List() }

// Log returns a run log (latest run by default).
func (s *Service) Log(id, runID string) ([]byte, error) {
	spec, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	if runID == "" {
		if spec.CurrentRunID != "" {
			runID = spec.CurrentRunID
		} else if len(spec.Runs) > 0 {
			runID = spec.Runs[len(spec.Runs)-1].ID
		} else {
			return nil, fmt.Errorf("no runs recorded")
		}
	}
	if !ValidID(runID) {
		return nil, fmt.Errorf("invalid run id")
	}
	data, err := s.store.ReadLog(id, runID)
	if err != nil {
		return nil, fmt.Errorf("log not available: %w", err)
	}
	return data, nil
}

// Outputs returns the output metadata plus the file list on disk.
func (s *Service) Outputs(id string) (*OutputMeta, []string, error) {
	if _, err := s.Get(id); err != nil {
		return nil, nil, err
	}
	meta, err := s.store.ReadOutputMarker(id)
	if err != nil || meta == nil {
		return nil, nil, fmt.Errorf("no successful output yet")
	}
	files := append([]string{}, meta.Files...)
	return meta, files, nil
}

// ---------------------------------------------------------------------------
// Scheduling and workers
// ---------------------------------------------------------------------------

func (s *Service) isScheduled(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.queued[id] || s.running[id]
}

// enqueue registers exactly one job in the queue. The caller must have
// already persisted the record with StatusQueued while holding the spec
// lock. enqueue itself only touches the scheduler maps and the channel, so
// it is safe to call with the spec lock held (schedule used to re-lock it,
// which deadlocked since sync.Mutex is not reentrant).
func (s *Service) enqueue(id string) error {
	s.mu.Lock()
	if s.deleted[id] {
		s.mu.Unlock()
		return ErrNotFound
	}
	if s.queued[id] || s.running[id] {
		s.mu.Unlock()
		return ErrConflict
	}
	s.queued[id] = true
	s.mu.Unlock()

	select {
	case s.queue <- id:
		return nil
	default:
		s.mu.Lock()
		delete(s.queued, id)
		s.mu.Unlock()
		return fmt.Errorf("job queue is full, try again later")
	}
}

func (s *Service) worker() {
	defer s.wg.Done()
	for {
		select {
		case <-s.closed:
			return
		case id := <-s.queue:
			s.mu.Lock()
			delete(s.queued, id)
			if s.deleted[id] {
				s.mu.Unlock()
				continue
			}
			s.running[id] = true
			s.mu.Unlock()
			s.runOne(id)
			s.mu.Lock()
			delete(s.running, id)
			delete(s.cancel, id)
			s.mu.Unlock()
		}
	}
}

func (s *Service) runOne(id string) {
	lk := s.specLock(id)
	lk.Lock()
	spec, err := s.store.Load(id)
	if err != nil {
		lk.Unlock()
		return
	}
	if spec.Status != StatusQueued {
		lk.Unlock()
		return
	}
	run := Run{
		ID:        newID("run"),
		SpecID:    id,
		Hash:      spec.CurrentHash,
		Status:    StatusProcessing,
		StartedAt: time.Now().UTC(),
	}
	now := time.Now().UTC()
	spec.Status = StatusProcessing
	spec.CurrentRunID = run.ID
	spec.LastAttemptAt = &now
	spec.UpdatedAt = now
	spec.Error = ""
	spec.Runs = appendBoundedRuns(spec.Runs, run, 20)
	if err := s.store.Save(spec); err != nil {
		lk.Unlock()
		return
	}
	targetHash := spec.CurrentHash
	lk.Unlock()

	// Set up run context, log file and directories outside the spec lock so
	// uploads of other versions are never blocked by a long generation.
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout)
	s.mu.Lock()
	if s.deleted[id] {
		s.mu.Unlock()
		cancel()
		return
	}
	s.cancel[id] = cancel
	s.mu.Unlock()
	defer cancel()

	logPath, err := s.store.LogPath(id, run.ID)
	if err != nil {
		cancel()
		s.finishRun(id, run.ID, targetHash, GenerateResult{}, 0, fmt.Errorf("prepare log: %w", err))
		return
	}
	logFile, err := os.Create(logPath)
	if err != nil {
		cancel()
		s.finishRun(id, run.ID, targetHash, GenerateResult{}, 0, fmt.Errorf("prepare log: %w", err))
		return
	}
	defer logFile.Close()

	source, err := s.store.ReadSource(id)
	if err != nil {
		fmt.Fprintf(logFile, "ERROR: read source: %v\n", err)
		s.finishRun(id, run.ID, targetHash, GenerateResult{}, 0, fmt.Errorf("read source: %w", err))
		return
	}
	parsed, verr := ParseAndValidate(source)
	if verr != nil {
		fmt.Fprintf(logFile, "ERROR: spec no longer valid: %v\n", verr)
		s.finishRun(id, run.ID, targetHash, GenerateResult{}, 0, verr)
		return
	}
	workspace, err := s.store.WorkspaceDir(id)
	if err != nil {
		fmt.Fprintf(logFile, "ERROR: prepare workspace: %v\n", err)
		s.finishRun(id, run.ID, targetHash, GenerateResult{}, 0, err)
		return
	}
	staging, err := s.store.StagingDir(id)
	if err != nil {
		fmt.Fprintf(logFile, "ERROR: prepare staging: %v\n", err)
		s.finishRun(id, run.ID, targetHash, GenerateResult{}, 0, err)
		return
	}

	result, genErr := s.gen.Generate(ctx, GenerateRequest{
		SpecID:    id,
		Parsed:    parsed,
		Workspace: workspace,
		Staging:   staging,
		Log:       logFile,
		ServerURL: s.serverURL,
	})

	if ctx.Err() == context.DeadlineExceeded && genErr == nil {
		genErr = fmt.Errorf("job timed out after %s", s.timeout)
	}
	exitCode := result.ExitCode
	if genErr != nil {
		if re, ok := genErr.(*RunError); ok {
			exitCode = re.ExitCode
		} else if ctx.Err() == context.DeadlineExceeded {
			exitCode = -1
		} else if exitCode == 0 {
			exitCode = 1
		}
	}
	_ = logFile.Sync()
	s.finishRun(id, run.ID, targetHash, result, exitCode, genErr)
}

// finishRun persists the outcome. A run for a stale hash (superseded by a
// newer upload while it was running) is recorded but never promotes outputs
// and never clears the newer version's Queued state.
func (s *Service) finishRun(id, runID, targetHash string, result GenerateResult, exitCode int, genErr error) {
	lk := s.specLock(id)
	lk.Lock()
	defer lk.Unlock()

	s.mu.Lock()
	isDeleted := s.deleted[id]
	s.mu.Unlock()
	if isDeleted {
		_ = os.RemoveAll(stagingPath(s.store, id))
		return
	}
	spec, err := s.store.Load(id)
	if err != nil {
		return
	}
	now := time.Now().UTC()
	superseded := spec.CurrentHash != targetHash

	// Locate the run record.
	idx := -1
	for i := range spec.Runs {
		if spec.Runs[i].ID == runID {
			idx = i
			break
		}
	}
	finishRun := Run{
		ID:        runID,
		SpecID:    id,
		Hash:      targetHash,
		StartedAt: now,
		EndedAt:   &now,
	}
	if genErr != nil {
		finishRun.Status = StatusFailed
		finishRun.Error = genErr.Error()
		ec := exitCode
		finishRun.ExitCode = &ec
	} else {
		finishRun.Status = StatusCompleted
		ec := exitCode
		finishRun.ExitCode = &ec
	}
	if idx >= 0 {
		finishRun.StartedAt = spec.Runs[idx].StartedAt
		spec.Runs[idx] = finishRun
	} else {
		spec.Runs = appendBoundedRuns(spec.Runs, finishRun, 20)
	}

	if genErr != nil {
		// Failure never touches the last good output (staging is left for
		// inspection, promotion is skipped). If a newer version arrived,
		// keep whatever state the newer scheduling set.
		if !superseded && spec.CurrentRunID == runID {
			spec.Status = StatusFailed
			spec.CurrentRunID = ""
			spec.Error = genErr.Error()
			spec.UpdatedAt = now
			spec.LastAttemptAt = &now
		}
		_ = s.store.Save(spec)
		return
	}

	// Success for a superseded hash: record only. The newer version keeps
	// its own lifecycle; last good output stays untouched.
	if superseded {
		_ = s.store.Save(spec)
		_ = os.RemoveAll(stagingPath(s.store, id))
		return
	}

	// Atomic promotion: outputs/ is replaced only after staging is complete.
	if err := s.store.PromoteStaging(id); err != nil {
		failed := fmt.Errorf("promote outputs: %w", err)
		if idx >= 0 {
			spec.Runs[idx].Status = StatusFailed
			spec.Runs[idx].Error = failed.Error()
		}
		if spec.CurrentRunID == runID {
			spec.Status = StatusFailed
			spec.CurrentRunID = ""
			spec.Error = failed.Error()
			spec.UpdatedAt = now
		}
		_ = s.store.Save(spec)
		return
	}
	meta := &OutputMeta{
		Hash:           targetHash,
		GeneratedAt:    now,
		RunID:          runID,
		Packages:       result.Packages,
		Files:          result.Files,
		ExampleProject: "workspace",
	}
	_ = s.store.WriteOutputMarker(id, meta)
	spec.Output = meta
	spec.Status = StatusCompleted
	spec.CurrentRunID = ""
	spec.Error = ""
	spec.LastGoodHash = targetHash
	spec.Stale = false
	spec.LastSuccessAt = &now
	spec.LastAttemptAt = &now
	spec.UpdatedAt = now
	_ = s.store.Save(spec)
}

func stagingPath(st *Store, id string) string {
	dir, err := st.specDir(id)
	if err != nil {
		return ""
	}
	return filepath.Join(dir, dirStaging)
}

// Recover re-queues interrupted jobs after a backend restart and removes
// orphan directories that have no metadata (they can never be promoted or
// listed). Idempotent and safe to call once at startup.
func (s *Service) Recover() {
	entries, err := os.ReadDir(s.store.Root)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(s.store.Root, e.Name(), fileMetadata)); err != nil {
			_ = os.RemoveAll(filepath.Join(s.store.Root, e.Name()))
		}
	}
	existing, err := s.store.List()
	if err != nil {
		return
	}
	for _, spec := range existing {
		if spec.Status == StatusProcessing || spec.Status == StatusQueued {
			lk := s.specLock(spec.ID)
			lk.Lock()
			fresh, err := s.store.Load(spec.ID)
			if err != nil {
				lk.Unlock()
				continue
			}
			now := time.Now().UTC()
			// Mark the interrupted run so history shows the restart.
			for i := range fresh.Runs {
				if fresh.Runs[i].Status == StatusProcessing {
					fresh.Runs[i].Status = StatusFailed
					fresh.Runs[i].Error = "interrupted by backend restart; re-queued"
					fresh.Runs[i].EndedAt = &now
				}
			}
			fresh.Status = StatusQueued
			fresh.CurrentRunID = ""
			fresh.UpdatedAt = now
			_ = s.store.Save(fresh)
			lk.Unlock()
			s.mu.Lock()
			if !s.queued[fresh.ID] && !s.running[fresh.ID] {
				s.queued[fresh.ID] = true
				s.mu.Unlock()
				select {
				case s.queue <- fresh.ID:
				default:
					// Queue full at startup: roll back to Uploaded so the
					// record never strands in Queued without a queued job;
					// an operator retry will schedule it.
					s.mu.Lock()
					delete(s.queued, fresh.ID)
					s.mu.Unlock()
					lk.Lock()
					if rec, err := s.store.Load(fresh.ID); err == nil {
						rec.Status = StatusUploaded
						_ = s.store.Save(rec)
					}
					lk.Unlock()
				}
			} else {
				s.mu.Unlock()
			}
		}
	}
}

func appendBoundedRuns(runs []Run, r Run, max int) []Run {
	runs = append(runs, r)
	if len(runs) > max {
		runs = runs[len(runs)-max:]
	}
	return runs
}
