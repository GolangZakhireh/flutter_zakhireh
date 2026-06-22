package dashboard

import (
	"fmt"
	"html/template"
	"net/http"

	"flutterzakhireh/internal/storage"
)

// DashboardHandler serves the web dashboard showing cached packages.
type DashboardHandler struct {
	Storage storage.Backend
	Tmpl    *template.Template
}

// NewDashboardHandler creates a dashboard handler with pre-loaded HTML templates.
func NewDashboardHandler(store storage.Backend) *DashboardHandler {
	funcMap := template.FuncMap{
		"humanBytes": humanBytes,
	}

	return &DashboardHandler{
		Storage: store,
		Tmpl: template.Must(template.New("index.html").Funcs(funcMap).ParseFiles(
			"internal/dashboard/templates/index.html",
		)),
	}
}

// humanBytes converts a byte count to a human-readable string (e.g. "1.5 Kib").
func humanBytes(b int64) string {
	const unit = 1024
	if b < unit {
		return fmt.Sprintf("%d B", b)
	}
	div, exp := int64(unit), 0
	for n := b / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cib", float64(b)/float64(div), "KMGTPE"[exp])
}

// ServeHTTP renders the dashboard page with a list of all cached packages.
func (h *DashboardHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var packages []storage.Package

	err := h.Storage.Walk(func(pkg storage.Package) error {
		packages = append(packages, pkg)
		return nil
	})

	if err != nil {
		http.Error(w, "internal server error", http.StatusInternalServerError)
		return
	}

	h.Tmpl.Execute(w, packages)
}
