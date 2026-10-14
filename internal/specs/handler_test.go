package specs

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func testHandler(t *testing.T, gen Generator) (*Handler, *Service) {
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
	return NewHandler(svc, 1<<20), svc
}

func uploadRequest(t *testing.T, filename, content string) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("spec", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(fw, content); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/specs", &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	return req
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %v (%q)", err, rec.Body.String())
	}
	return body
}

func specIDOf(t *testing.T, body map[string]any) string {
	t.Helper()
	spec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("missing spec in %v", body)
	}
	id, _ := spec["id"].(string)
	if id == "" {
		t.Fatalf("missing spec id in %v", body)
	}
	return id
}

const handlerSpec = "name: web_app\ndependencies:\n  http: ^1.1.0\n"

func TestAPIUploadListDetail(t *testing.T) {
	h, svc := testHandler(t, &fakeGen{})

	rec := httptest.NewRecorder()
	h.upload(rec, uploadRequest(t, "web.yaml", handlerSpec))
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d (%s)", rec.Code, rec.Body.String())
	}
	id := specIDOf(t, decodeBody(t, rec))
	if err := waitStatus(svc, id, StatusCompleted); err != nil {
		t.Fatal(err)
	}

	// Identical upload after success: deduped, no regen.
	rec = httptest.NewRecorder()
	h.upload(rec, uploadRequest(t, "web.yaml", handlerSpec))
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 deduped, got %d", rec.Code)
	}
	if body := decodeBody(t, rec); body["deduped"] != true {
		t.Errorf("expected deduped=true, got %v", body)
	}

	// List preserves state.
	req := httptest.NewRequest(http.MethodGet, "/api/specs", nil)
	rec = httptest.NewRecorder()
	h.list(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if body := decodeBody(t, rec); body["count"].(float64) != 1 {
		t.Errorf("expected count 1, got %v", body)
	}

	// Detail + status + outputs + logs.
	for path, fn := range map[string]func(http.ResponseWriter, *http.Request){
		"/api/specs/" + id: func(w http.ResponseWriter, r *http.Request) { h.detail(w, r, id) },
	} {
		_ = path
		rec = httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+id, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("expected 200 for detail, got %d", rec.Code)
		}
	}
	rec = httptest.NewRecorder()
	h.status(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+id+"/status", nil), id)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 status, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.outputs(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+id+"/outputs", nil), id)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 outputs, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.logs(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+id+"/logs", nil), id)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "fake generator ok") {
		t.Fatalf("expected run log, got %d %q", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.download(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+id+"/outputs/download", nil), id)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/gzip" {
		t.Fatalf("expected tar.gz download, got %d", rec.Code)
	}
}

func TestAPIUploadValidation(t *testing.T) {
	h, _ := testHandler(t, &fakeGen{})
	cases := []struct {
		name     string
		filename string
		content  string
		wantCode int
	}{
		{"bad extension", "spec.json", handlerSpec, http.StatusBadRequest},
		{"bad yaml", "spec.yaml", "name: [broken", http.StatusBadRequest},
		{"missing name", "spec.yaml", "dependencies:\n  http: x\n", http.StatusBadRequest},
		{"empty", "spec.yaml", "", http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.upload(rec, uploadRequest(t, tc.filename, tc.content))
			if rec.Code != tc.wantCode {
				t.Errorf("expected %d, got %d (%s)", tc.wantCode, rec.Code, rec.Body.String())
			}
			if body := decodeBody(t, rec); body["error"] == nil {
				t.Error("expected structured error response")
			}
		})
	}
	// Missing form field.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/specs", strings.NewReader("x=1"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	h.upload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for missing field, got %d", rec.Code)
	}
}

func TestAPIRetryConflictAndDelete(t *testing.T) {
	block := make(chan struct{})
	h, svc := testHandler(t, &fakeGen{block: block})

	rec := httptest.NewRecorder()
	h.upload(rec, uploadRequest(t, "web.yaml", handlerSpec))
	id := specIDOf(t, decodeBody(t, rec))
	if err := waitStatus(svc, id, StatusProcessing); err != nil {
		t.Fatal(err)
	}
	// Retry while running conflicts.
	rec = httptest.NewRecorder()
	h.retry(rec, httptest.NewRequest(http.MethodPost, "/api/specs/"+id+"/retry", nil), id)
	if rec.Code != http.StatusConflict {
		t.Errorf("expected 409 retry conflict, got %d", rec.Code)
	}
	close(block)
	if err := waitStatus(svc, id, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	// Retry after success re-queues.
	rec = httptest.NewRecorder()
	h.retry(rec, httptest.NewRequest(http.MethodPost, "/api/specs/"+id+"/retry", nil), id)
	if rec.Code != http.StatusAccepted {
		t.Errorf("expected 202 retry, got %d (%s)", rec.Code, rec.Body.String())
	}
	if err := waitStatus(svc, id, StatusCompleted); err != nil {
		t.Fatal(err)
	}
	// Delete with confirmation semantics: record + files gone.
	rec = httptest.NewRecorder()
	h.remove(rec, httptest.NewRequest(http.MethodDelete, "/api/specs/"+id, nil), id)
	if rec.Code != http.StatusNoContent {
		t.Errorf("expected 204, got %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.detail(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+id, nil), id)
	if rec.Code != http.StatusNotFound {
		t.Errorf("expected 404 after delete, got %d", rec.Code)
	}
	// Unknown ids and traversal attempts are 404, never filesystem access.
	for _, bad := range []string{"../etc", "..", "nope!", ""} {
		rec = httptest.NewRecorder()
		h.routeOne(rec, httptest.NewRequest(http.MethodGet, "/api/specs/"+bad, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("expected 404 for %q, got %d", bad, rec.Code)
		}
	}
}
