package dashboard

import (
	"fmt"
	"html/template"
	"net/http"

	"flutterzakhireh/internal/specs"
	"flutterzakhireh/internal/storage"
)

// DashboardData is the view model for the combined packages + specs page.
type DashboardData struct {
	Packages  []storage.Package
	Specs     []*specs.Spec
	SpecCount int
}

// DashboardHandler serves the web dashboard showing cached packages and
// managed POP Spec files.
type DashboardHandler struct {
	Storage storage.Backend
	Specs   *specs.Service
	Tmpl    *template.Template
}

// NewDashboardHandler creates a dashboard handler with pre-loaded HTML templates.
func NewDashboardHandler(store storage.Backend, svc *specs.Service) *DashboardHandler {
	funcMap := template.FuncMap{
		"humanBytes": humanBytes,
	}

	return &DashboardHandler{
		Storage: store,
		Specs:   svc,
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

// ServeHTTP renders the dashboard page with cached packages and spec records.
// Spec listing is best-effort: a specs failure never hides the package cache.
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

	data := DashboardData{Packages: packages}
	if h.Specs != nil {
		if list, err := h.Specs.List(); err == nil {
			data.Specs = list
			data.SpecCount = len(list)
		}
	}

	h.Tmpl.Execute(w, data)
}
