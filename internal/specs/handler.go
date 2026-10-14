package specs

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Handler exposes the minimum spec-management API. It extends the existing
// net/http ServeMux conventions (no second framework).
//
//	POST   /api/specs                        upload a POP Spec YAML
//	GET    /api/specs                        list all specs
//	GET    /api/specs/{id}                   spec details
//	GET    /api/specs/{id}/status            lightweight polling status
//	GET    /api/specs/{id}/source            download stored source.yaml
//	GET    /api/specs/{id}/logs[?run=]       run log (latest by default)
//	POST   /api/specs/{id}/retry             schedule a new run
//	DELETE /api/specs/{id}                   delete spec + managed files
//	GET    /api/specs/{id}/outputs           output metadata
//	GET    /api/specs/{id}/outputs/download  outputs as .tar.gz
type Handler struct {
	Service   *Service
	MaxUpload int64
}

// NewHandler creates the API handler.
func NewHandler(svc *Service, maxUpload int64) *Handler {
	if maxUpload <= 0 {
		maxUpload = 1 << 20
	}
	return &Handler{Service: svc, MaxUpload: maxUpload}
}

// Register mounts the handler on mux.
func (h *Handler) Register(mux *http.ServeMux) {
	mux.HandleFunc("/api/specs", h.routeRoot)
	mux.HandleFunc("/api/specs/", h.routeOne)
}

func (h *Handler) routeRoot(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		h.upload(w, r)
	case http.MethodGet:
		h.list(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *Handler) routeOne(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/specs/")
	parts := strings.Split(rest, "/")
	id := parts[0]
	if !ValidID(id) {
		writeErr(w, http.StatusNotFound, "unknown spec", "")
		return
	}
	sub := ""
	if len(parts) > 1 {
		sub = strings.Join(parts[1:], "/")
	}
	switch {
	case sub == "" && r.Method == http.MethodGet:
		h.detail(w, r, id)
	case sub == "" && r.Method == http.MethodDelete:
		h.remove(w, r, id)
	case sub == "status" && r.Method == http.MethodGet:
		h.status(w, r, id)
	case sub == "source" && r.Method == http.MethodGet:
		h.source(w, r, id)
	case sub == "logs" && r.Method == http.MethodGet:
		h.logs(w, r, id)
	case sub == "retry" && r.Method == http.MethodPost:
		h.retry(w, r, id)
	case sub == "outputs" && r.Method == http.MethodGet:
		h.outputs(w, r, id)
	case sub == "outputs/download" && r.Method == http.MethodGet:
		h.download(w, r, id)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// upload handles multipart spec files. The form field is "spec".
func (h *Handler) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, h.MaxUpload+1<<20)
	if err := r.ParseMultipartForm(h.MaxUpload + (1 << 20)); err != nil {
		writeErr(w, http.StatusBadRequest, "failed to parse upload: "+err.Error(), "file")
		return
	}
	f, hdr, err := r.FormFile("spec")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing file field 'spec': attach the YAML as multipart field 'spec'", "spec")
		return
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, h.MaxUpload+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "failed to read upload: "+err.Error(), "file")
		return
	}
	if int64(len(data)) > h.MaxUpload {
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("file too large: limit %d bytes", h.MaxUpload), "file")
		return
	}
	spec, outcome, err := h.Service.Upload(hdr.Filename, data)
	if err != nil {
		var verr *ValidationError
		if errors.As(err, &verr) {
			writeErr(w, http.StatusBadRequest, verr.Message, verr.Field)
			return
		}
		if errors.Is(err, ErrConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": "this exact version is already being processed",
				"spec":  publicSpec(spec),
			})
			return
		}
		writeErr(w, http.StatusInternalServerError, "upload failed: "+err.Error(), "")
		return
	}
	status := http.StatusCreated
	body := map[string]any{"spec": publicSpec(spec)}
	if outcome.Deduped {
		status = http.StatusOK
		body["deduped"] = true
		body["message"] = "identical content already processed; no regeneration scheduled"
	}
	writeJSON(w, status, body)
}

func (h *Handler) list(w http.ResponseWriter, r *http.Request) {
	specs, err := h.Service.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	out := make([]any, 0, len(specs))
	counts := map[string]int{}
	for _, s := range specs {
		counts[s.Status]++
		out = append(out, publicSpec(s))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"specs":  out,
		"count":  len(out),
		"status": counts,
	})
}

func (h *Handler) detail(w http.ResponseWriter, r *http.Request, id string) {
	spec, err := h.Service.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "spec not found", "")
		return
	}
	changed := spec.LastGoodHash != "" && spec.CurrentHash != spec.LastGoodHash
	writeJSON(w, http.StatusOK, map[string]any{
		"spec":                  publicSpec(spec),
		"changed_since_success": changed,
		"outputs_valid":         h.Service.store.OutputsValid(id, spec.CurrentHash),
	})
}

func (h *Handler) status(w http.ResponseWriter, r *http.Request, id string) {
	spec, err := h.Service.Get(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "spec not found", "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id":              spec.ID,
		"status":          spec.Status,
		"stale":           spec.Stale,
		"error":           spec.Error,
		"current_run_id":  spec.CurrentRunID,
		"updated_at":      spec.UpdatedAt.Format(time.RFC3339),
		"last_attempt_at": formatTime(spec.LastAttemptAt),
		"last_success_at": formatTime(spec.LastSuccessAt),
	})
}

func (h *Handler) source(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.Service.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, "spec not found", "")
		return
	}
	data, err := h.Service.store.ReadSource(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "source not available", "")
		return
	}
	w.Header().Set("Content-Type", "application/yaml")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s.yaml", id))
	_, _ = w.Write(data)
}

func (h *Handler) logs(w http.ResponseWriter, r *http.Request, id string) {
	data, err := h.Service.Log(id, r.URL.Query().Get("run"))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error(), "")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func (h *Handler) retry(w http.ResponseWriter, r *http.Request, id string) {
	spec, err := h.Service.Retry(id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "spec not found", "")
			return
		}
		if errors.Is(err, ErrConflict) {
			writeErr(w, http.StatusConflict, "a job is already queued or running for this version", "")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"spec": publicSpec(spec)})
}

func (h *Handler) remove(w http.ResponseWriter, r *http.Request, id string) {
	if err := h.Service.Delete(id); err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "spec not found", "")
			return
		}
		writeErr(w, http.StatusInternalServerError, "delete failed: "+err.Error(), "")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *Handler) outputs(w http.ResponseWriter, r *http.Request, id string) {
	meta, files, err := h.Service.Outputs(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error(), "")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"output": meta, "files": files})
}

// download streams outputs/ as a .tar.gz built only from files inside the
// spec's own directory (no absolute paths, no symlinks followed).
func (h *Handler) download(w http.ResponseWriter, r *http.Request, id string) {
	if _, err := h.Service.Get(id); err != nil {
		writeErr(w, http.StatusNotFound, "spec not found", "")
		return
	}
	dir, err := h.Service.store.OutputsDir(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error(), "")
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) == 0 {
		writeErr(w, http.StatusNotFound, "no successful output yet", "")
		return
	}
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-outputs.tar.gz", id))
	gz := gzip.NewWriter(w)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		p := filepath.Join(dir, e.Name())
		if rel, err := filepath.Rel(dir, p); err != nil || strings.HasPrefix(rel, "..") {
			continue
		}
		info, err := e.Info()
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		_ = tw.WriteHeader(&tar.Header{Name: e.Name(), Mode: 0o644, Size: int64(len(data))})
		_, _ = tw.Write(data)
	}
}

// publicSpec strips anything that must not leave the server. Paths stay
// server-side: only stable relative markers are exposed.
func publicSpec(s *Spec) map[string]any {
	if s == nil {
		return nil
	}
	var out *OutputMeta
	if s.Output != nil {
		cp := *s.Output
		out = &cp
	}
	runs := make([]Run, len(s.Runs))
	copy(runs, s.Runs)
	for i := range runs {
		runs[i].LogFile = ""
	}
	return map[string]any{
		"id":                s.ID,
		"original_filename": s.OriginalFilename,
		"name":              s.Name,
		"current_hash":      shortHash(s.CurrentHash),
		"stale":             s.Stale,
		"status":            s.Status,
		"error":             s.Error,
		"uploaded_at":       s.UploadedAt.Format(time.RFC3339),
		"updated_at":        s.UpdatedAt.Format(time.RFC3339),
		"last_attempt_at":   formatTime(s.LastAttemptAt),
		"last_success_at":   formatTime(s.LastSuccessAt),
		"current_run_id":    s.CurrentRunID,
		"output":            out,
		"runs":              runs,
	}
}

func shortHash(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

func formatTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.Format(time.RFC3339)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg, field string) {
	body := map[string]any{"error": msg}
	if field != "" {
		body["field"] = field
	}
	writeJSON(w, status, body)
}
