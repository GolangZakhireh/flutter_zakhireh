package upload

import (
	"fmt"
	"log"
	"net/http"

	"flutterzakhireh/internal/storage"
)

// UploadHandler handles POST requests to upload package files (.tar.gz, .json).
type UploadHandler struct {
	Storage storage.Backend
}

// NewUploadHandler creates a new upload handler backed by the given storage.
func NewUploadHandler(store storage.Backend) *UploadHandler {
	return &UploadHandler{Storage: store}
}

// ServeHTTP handles a package upload request.
// Expects a multipart POST with fields: package, version, archive, metadata.
func (h *UploadHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	packageName := r.FormValue("package")
	version := r.FormValue("version")

	if packageName == "" || version == "" {
		http.Error(w, "package and version required", http.StatusBadRequest)
		return
	}

	// Save archive (.tar.gz)
	archiveFile, _, err := r.FormFile("archive")
	if err != nil {
		http.Error(w, fmt.Sprintf("failed getting archive: %v", err), http.StatusBadRequest)
		return
	}
	defer archiveFile.Close()

	if err := h.Storage.Save(packageName, version, ".tar.gz", archiveFile); err != nil {
		http.Error(w, fmt.Sprintf("failed saving archive: %v", err), http.StatusInternalServerError)
		return
	}

	// Save metadata (optional)
	metadataFile, _, err := r.FormFile("metadata")
	if err == nil && metadataFile != nil {
		defer metadataFile.Close()
		if err := h.Storage.Save(packageName, version, ".json", metadataFile); err != nil {
			log.Printf("[UPLOAD] Warning: failed saving metadata for %s@%s: %v", packageName, version, err)
		}
	}

	// Update versions list
	versions, err := h.Storage.ListVersions(packageName)
	if err != nil {
		log.Printf("[UPLOAD] Warning: failed to list versions for %s: %v", packageName, err)
	} else {
		// Check if version already in list
		found := false
		for _, v := range versions {
			if v == version {
				found = true
				break
			}
		}
		if !found {
			versions = append(versions, version)
			if err := h.Storage.SaveMetadata(packageName, versions); err != nil {
				log.Printf("[UPLOAD] Warning: failed to save metadata for %s: %v", packageName, err)
			}
		}
	}

	log.Printf("[UPLOAD] %s@%s uploaded by %s", packageName, version, r.RemoteAddr)
	w.WriteHeader(http.StatusOK)
	fmt.Fprintln(w, "ok")
}
