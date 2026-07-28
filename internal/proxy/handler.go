package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"flutterzakhireh/internal/storage"
)

// File extensions used in the Flutter package protocol.
const (
	ExtTarGz = ".tar.gz" // Package archive
	ExtJSON  = ".json"   // Package metadata
)

// PubDevAPIResponse mirrors the upstream pub.dev response for
// GET /api/packages/<name>.
//
// IMPORTANT: "versions" is a JSON ARRAY of objects (not a map).
type PubDevAPIResponse struct {
	Name        string          `json:"name"`
	Latest      PubDevVersion   `json:"latest"`
	Versions    []PubDevVersion `json:"versions"`
	Publisher   string          `json:"publisher,omitempty"`
	Published   string          `json:"published,omitempty"`
	Updated     string          `json:"updated,omitempty"`
	Description string          `json:"description,omitempty"`
}

// PubDevVersion mirrors a single version entry in pub.dev responses.
type PubDevVersion struct {
	Version    string         `json:"version"`
	Pubspec    map[string]any `json:"pubspec,omitempty"`
	ArchiveURL string         `json:"archive_url,omitempty"`
	ArchiveSha string         `json:"archive_sha256,omitempty"`
	Published  string         `json:"published,omitempty"`
}

// ProxyHandler serves Flutter package requests following the pub.dev API protocol.
// It serves cached packages from local storage first, then falls back to the
// upstream pub.dev, transparently caching the upstream response for next time.
type ProxyHandler struct {
	Storage    storage.Backend // Local package cache
	Fallback   string          // Upstream proxy URL (e.g. https://pub.dev)
	HttpClient *http.Client    // HTTP client for upstream requests
	Validator  *Validator      // Access control validator
}

// NewProxyHandler creates a new proxy handler with the given storage, upstream URL, and validator.
func NewProxyHandler(store storage.Backend, fallback string, validator *Validator) *ProxyHandler {
	return &ProxyHandler{
		Storage:    store,
		Fallback:   fallback,
		HttpClient: &http.Client{},
		Validator:  validator,
	}
}

// ServeHTTP handles incoming package requests.
// Supports pub.dev API endpoints:
//
//	/api/packages/<package>                          - Package metadata (all versions)
//	/api/packages/<package>/versions/<version>       - Specific version metadata
//	/api/archives/<package>-<version>.tar.gz         - Package archive (canonical)
//	/packages/<package>/versions/<version>.tar.gz    - Package archive (legacy redirect target)
func (h *ProxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqPath := strings.TrimPrefix(r.URL.Path, "/")

	if !h.Validator.IsAllowed(packageNameForPath(reqPath)) {
		http.Error(w, "package access denied", http.StatusForbidden)
		return
	}

	switch {
	// Package (all-versions) metadata: /api/packages/<package>
	case strings.HasPrefix(reqPath, "api/packages/") && !strings.Contains(reqPath, "/versions/"):
		packageName := strings.TrimSuffix(strings.TrimPrefix(reqPath, "api/packages/"), "/")
		if packageName == "" {
			http.Error(w, "invalid package name", http.StatusBadRequest)
			return
		}
		h.handlePackageMetadata(w, r, packageName)
		return

	// Version metadata: /api/packages/<package>/versions/<version>
	case strings.HasPrefix(reqPath, "api/packages/") && strings.Contains(reqPath, "/versions/"):
		parts := strings.SplitN(reqPath, "/versions/", 2)
		if len(parts) != 2 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		packageName := strings.TrimPrefix(parts[0], "api/packages/")
		version := strings.TrimSuffix(parts[1], "/")
		if packageName == "" || version == "" {
			http.Error(w, "invalid package or version", http.StatusBadRequest)
			return
		}
		h.handleVersionMetadata(w, r, packageName, version)
		return

	// Canonical archive path: /api/archives/<package>-<version>.tar.gz
	case strings.HasPrefix(reqPath, "api/archives/") && strings.HasSuffix(reqPath, ".tar.gz"):
		packageName, version, ok := splitArchiveName(strings.TrimSuffix(strings.TrimPrefix(reqPath, "api/archives/"), ".tar.gz"))
		if !ok {
			http.Error(w, "invalid archive name", http.StatusBadRequest)
			return
		}
		h.handlePackageArchive(w, r, packageName, version)
		return

	// Legacy archive path: /packages/<package>/versions/<version>.tar.gz
	case strings.HasPrefix(reqPath, "packages/") && strings.HasSuffix(reqPath, ".tar.gz"):
		parts := strings.SplitN(reqPath, "/versions/", 2)
		if len(parts) != 2 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		packageName := strings.TrimPrefix(parts[0], "packages/")
		version := strings.TrimSuffix(parts[1], ".tar.gz")
		if packageName == "" || version == "" {
			http.Error(w, "invalid package or version", http.StatusBadRequest)
			return
		}
		// pub.dev 303-redirects this path to /api/archives/<package>-<version>.tar.gz;
		// we serve the content directly instead.
		h.handlePackageArchive(w, r, packageName, version)
		return
	}

	http.Error(w, "not found", http.StatusNotFound)
}

// IsPackageRequest reports whether a path targets the package API.
func IsPackageRequest(path string) bool {
	p := strings.TrimPrefix(path, "/")
	return strings.HasPrefix(p, "api/packages/") ||
		strings.HasPrefix(p, "api/archives/") ||
		strings.HasPrefix(p, "packages/")
}

// packageNameForPath extracts the package name segment (best-effort) for
// allow/deny checks. Returns "" for non-package paths (always allowed).
func packageNameForPath(path string) string {
	p := strings.TrimPrefix(path, "/")
	for _, prefix := range []string{"api/packages/", "packages/"} {
		if strings.HasPrefix(p, prefix) {
			rest := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
			if i := strings.Index(rest, "/versions/"); i >= 0 {
				return rest[:i]
			}
			return rest
		}
	}
	if strings.HasPrefix(p, "api/archives/") {
		name, _, ok := splitArchiveName(strings.TrimSuffix(strings.TrimPrefix(p, "api/archives/"), ".tar.gz"))
		if ok {
			return name
		}
	}
	return ""
}

// splitArchiveName splits "<package>-<version>" from an archive filename.
// pub.dev names archives "<package>-<version>.tar.gz" where version is the
// last dash-separated segment.
func splitArchiveName(name string) (string, string, bool) {
	if name == "" {
		return "", "", false
	}
	idx := strings.LastIndex(name, "-")
	if idx <= 0 {
		return "", "", false
	}
	return name[:idx], name[idx+1:], true
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

// handlePackageMetadata serves GET /api/packages/<package>.
func (h *ProxyHandler) handlePackageMetadata(w http.ResponseWriter, r *http.Request, packageName string) {
	// 1. Serve from cache.
	if data, ok, err := h.Storage.GetPackageMeta(packageName); err != nil {
		http.Error(w, fmt.Sprintf("storage error: %v", err), http.StatusInternalServerError)
		return
	} else if ok {
		writeJSON(w, http.StatusOK, data)
		return
	}

	// 2. Fall back to upstream.
	body, ok := h.fetchJSON(w, "/api/packages/%s", packageName)
	if !ok {
		return // error already written
	}

	// 3. Rewrite archive_urls so the client downloads archives from us.
	rewritten := rewritePackageArchiveURLs(body, r)

	// 4. Cache for next time (best-effort).
	if err := h.Storage.SavePackageMeta(packageName, rewritten); err != nil {
		slog.Warn("failed to cache package metadata", "package", packageName, "error", err)
	}
	h.persistVersionsFromPackageMeta(packageName, rewritten) // keeps dashboard in sync

	writeJSON(w, http.StatusOK, rewritten)
}

// handleVersionMetadata serves GET /api/packages/<package>/versions/<version>.
func (h *ProxyHandler) handleVersionMetadata(w http.ResponseWriter, r *http.Request, packageName, version string) {
	if data, ok, err := h.Storage.GetVersionMeta(packageName, version); err != nil {
		http.Error(w, fmt.Sprintf("storage error: %v", err), http.StatusInternalServerError)
		return
	} else if ok {
		writeJSON(w, http.StatusOK, data)
		return
	}

	body, ok := h.fetchJSON(w, "/api/packages/%s/versions/%s", packageName, version)
	if !ok {
		return
	}

	rewritten := rewriteVersionArchiveURL(body, r, packageName, version)
	if err := h.Storage.SaveVersionMeta(packageName, version, rewritten); err != nil {
		slog.Warn("failed to cache version metadata", "package", packageName, "version", version, "error", err)
	}
	writeJSON(w, http.StatusOK, rewritten)
}

// handlePackageArchive serves the .tar.gz archive for a package@version.
// Serves from cache when present; otherwise downloads, caches, and streams.
func (h *ProxyHandler) handlePackageArchive(w http.ResponseWriter, r *http.Request, packageName, version string) {
	// 1. Serve cached archive if present.
	if exists, err := h.Storage.Exists(packageName, version, ExtTarGz); err != nil {
		http.Error(w, fmt.Sprintf("storage error: %v", err), http.StatusInternalServerError)
		return
	} else if exists {
		content, err := h.Storage.Get(packageName, version, ExtTarGz)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to read archive: %v", err), http.StatusInternalServerError)
			return
		}
		defer content.Close()
		serveArchive(w, content, packageName, version)
		return
	}

	// 2. Resolve the real upstream archive URL.
	archiveURL, ok := h.resolveUpstreamArchiveURL(w, r, packageName, version)
	if !ok {
		return // error already written
	}

	// 3. Download from upstream.
	resp, err := h.HttpClient.Get(archiveURL)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		http.Error(w, "archive not found upstream", resp.StatusCode)
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read archive: %v", err), http.StatusBadGateway)
		return
	}

	// 4. Cache (best-effort) so subsequent requests are served locally.
	if err := h.Storage.Save(packageName, version, ExtTarGz, bytes.NewReader(body)); err != nil {
		slog.Warn("failed to cache archive", "package", packageName, "version", version, "error", err)
	}

	serveArchive(w, bytes.NewReader(body), packageName, version)
}

// serveArchive streams a .tar.gz archive to the client.
func serveArchive(w http.ResponseWriter, r io.Reader, packageName, version string) {
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s-%s.tar.gz", packageName, version))
	if _, err := io.Copy(w, r); err != nil {
		slog.Warn("failed to stream archive", "package", packageName, "error", err)
	}
}

// resolveUpstreamArchiveURL obtains the upstream archive_url for a version.
// Priority:
//  1. Cached version metadata (archive_url may be rewritten to proxy — reconstructed)
//  2. Direct URL construction from the known pub.dev pattern (avoids API call)
//  3. Upstream version metadata API (last resort)
func (h *ProxyHandler) resolveUpstreamArchiveURL(w http.ResponseWriter, r *http.Request, packageName, version string) (string, bool) {
	// 1. Prefer cached version metadata (no upstream round-trip).
	if data, ok, err := h.Storage.GetVersionMeta(packageName, version); err == nil && ok {
		if url := extractArchiveURL(data); url != "" {
			return reconstructUpstreamURL(url, h.Fallback, packageName, version), true
		}
	}

	// 2. Construct the upstream archive URL directly from the known pattern.
	//    This avoids calling the pub.dev version metadata API entirely.
	if h.Fallback != "" {
		return fmt.Sprintf("%s/api/archives/%s-%s.tar.gz", strings.TrimSuffix(h.Fallback, "/"), packageName, version), true
	}

	// 3. Ask upstream directly (last resort).
	body, ok := h.fetchJSON(w, "/api/packages/%s/versions/%s", packageName, version)
	if !ok {
		return "", false
	}
	url := extractArchiveURL(body)
	if url == "" {
		http.Error(w, "archive URL not found upstream", http.StatusNotFound)
		return "", false
	}
	return absoluteUpstreamURL(url, h.Fallback), true
}

// reconstructUpstreamURL extracts the upstream URL from a potentially
// proxy-rewritten archive_url by re-building it from the package name and version.
func reconstructUpstreamURL(url, fallback, packageName, version string) string {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return absoluteUpstreamURL(url, fallback)
	}
	if fallback == "" {
		return url
	}
	return fmt.Sprintf("%s/api/archives/%s-%s.tar.gz", strings.TrimSuffix(fallback, "/"), packageName, version)
}

// absoluteUpstreamURL turns a possibly-relative upstream archive_url into an
// absolute URL pointing at the upstream host.
func absoluteUpstreamURL(url, fallback string) string {
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return url
	}
	return strings.TrimSuffix(fallback, "/") + url
}

// extractArchiveURL pulls archive_url out of a version metadata blob.
func extractArchiveURL(body []byte) string {
	var v PubDevVersion
	if err := json.Unmarshal(body, &v); err != nil {
		return ""
	}
	return v.ArchiveURL
}

// ---------------------------------------------------------------------------
// Upstream fetching helper
// ---------------------------------------------------------------------------

// fetchJSON GETs an upstream JSON metadata endpoint. The format string's first
// verb is the upstream base URL; remaining args are URL path components.
// On error it writes the HTTP response to the client and returns ok=false.
func (h *ProxyHandler) fetchJSON(w http.ResponseWriter, format string, args ...any) ([]byte, bool) {
	if h.Fallback == "" {
		http.Error(w, "package not found (no upstream configured)", http.StatusNotFound)
		return nil, false
	}
	url := fmt.Sprintf("%s"+format, append([]any{strings.TrimSuffix(h.Fallback, "/")}, args...)...)
	resp, err := h.HttpClient.Get(url)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream error: %v", err), http.StatusBadGateway)
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		http.Error(w, "package not found upstream", http.StatusNotFound)
		return nil, false
	}
	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("upstream returned %d", resp.StatusCode), http.StatusBadGateway)
		return nil, false
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to read upstream body: %v", err), http.StatusBadGateway)
		return nil, false
	}
	return body, true
}

// ---------------------------------------------------------------------------
// archive_url rewriting (so the client downloads archives from US, not pub.dev)
// ---------------------------------------------------------------------------

// rewritePackageArchiveURLs rewrites every archive_url in a package metadata
// blob to point at this proxy's /api/archives/<package>-<version>.tar.gz.
func rewritePackageArchiveURLs(body []byte, r *http.Request) []byte {
	var resp PubDevAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return body // can't parse; leave unchanged
	}
	base := proxyBaseURL(r)
	for i := range resp.Versions {
		if v := &resp.Versions[i]; v.Version != "" {
			v.ArchiveURL = fmt.Sprintf("%s/api/archives/%s-%s.tar.gz", base, resp.Name, v.Version)
		}
	}
	if resp.Latest.Version != "" {
		resp.Latest.ArchiveURL = fmt.Sprintf("%s/api/archives/%s-%s.tar.gz", base, resp.Name, resp.Latest.Version)
	}
	if out, err := json.Marshal(resp); err == nil {
		return out
	}
	return body
}

// rewriteVersionArchiveURL rewrites the single archive_url in a version
// metadata blob.
func rewriteVersionArchiveURL(body []byte, r *http.Request, packageName, version string) []byte {
	var v PubDevVersion
	if err := json.Unmarshal(body, &v); err != nil {
		return body
	}
	v.ArchiveURL = fmt.Sprintf("%s/api/archives/%s-%s.tar.gz", proxyBaseURL(r), packageName, version)
	if out, err := json.Marshal(v); err == nil {
		return out
	}
	return body
}

// proxyBaseURL returns scheme://host[:port] clients should use to reach us.
// Honours X-Forwarded-* so it works behind a reverse proxy.
func proxyBaseURL(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if x := r.Header.Get("X-Forwarded-Proto"); x != "" {
		scheme = x
	}
	host := r.Host
	if x := r.Header.Get("X-Forwarded-Host"); x != "" {
		host = x
	}
	return scheme + "://" + host
}

// persistVersionsFromPackageMeta keeps the legacy versions.txt in sync so the
// dashboard reflects packages whose metadata (but not yet archives) is cached.
func (h *ProxyHandler) persistVersionsFromPackageMeta(packageName string, body []byte) {
	var resp PubDevAPIResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return
	}
	versions := make([]string, 0, len(resp.Versions))
	for _, v := range resp.Versions {
		versions = append(versions, v.Version)
	}
	if len(versions) > 0 {
		_ = h.Storage.SaveMetadata(packageName, versions)
	}
}

// ---------------------------------------------------------------------------
// write helper
// ---------------------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, data []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
