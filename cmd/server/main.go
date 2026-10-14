package main

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"flutterzakhireh/internal/config"
	"flutterzakhireh/internal/dashboard"
	"flutterzakhireh/internal/proxy"
	"flutterzakhireh/internal/server"
	"flutterzakhireh/internal/specs"
	"flutterzakhireh/internal/storage"
	"flutterzakhireh/internal/upload"
)

func main() {
	// File logging is best-effort: if the log directory cannot be created
	// (e.g. read-only working directory in a container), the server still
	// starts with stdout-only logging (captured by `docker logs`).
	// Override with FLUTTERZAKHIREH_LOG_DIR; empty disables file logging.
	logDest := io.Writer(os.Stdout)
	if logDir := os.Getenv("FLUTTERZAKHIREH_LOG_DIR"); logDir != "off" {
		if logDir == "" {
			logDir = "logs"
		}
		if err := os.MkdirAll(logDir, 0755); err != nil {
			fmt.Fprintf(os.Stderr, "WARNING: cannot create log directory %q: %v; using stdout only\n", logDir, err)
		} else {
			timestamp := time.Now().Format("2006-01-02_15-04-05")
			logFileName := filepath.Join(logDir, fmt.Sprintf("flutterzakhireh_%s.log", timestamp))
			if logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666); err != nil {
				fmt.Fprintf(os.Stderr, "WARNING: cannot open log file %q: %v; using stdout only\n", logFileName, err)
			} else {
				defer logFile.Close()
				logDest = io.MultiWriter(os.Stdout, logFile)
			}
		}
	}
	log.SetOutput(logDest)

	// Print ASCII banner
	log.Println(`
	███████╗██╗     ██╗   ██╗████████╗████████╗███████╗██████╗     ███████╗ █████╗ ██╗  ██╗██╗  ██╗██╗██████╗ ███████╗██╗  ██╗
	██╔════╝██║     ██║   ██║╚══██╔══╝╚══██╔══╝██╔════╝██╔══██╗    ╚══███╔╝██╔══██╗██║ ██╔╝██║  ██║██║██╔══██╗██╔════╝██║  ██║
	█████╗  ██║     ██║   ██║   ██║      ██║   █████╗  ██████╔╝      ███╔╝ ███████║█████╔╝ ███████║██║██████╔╝█████╗  ███████║
	██╔══╝  ██║     ██║   ██║   ██║      ██║   ██╔══╝  ██╔══██╗     ███╔╝  ██╔══██║██╔═██╗ ██╔══██║██║██╔══██╗██╔══╝  ██╔══██║
	██║     ███████╗╚██████╔╝   ██║      ██║   ███████╗██║  ██║    ███████╗██║  ██║██║  ██╗██║  ██║██║██║  ██║███████╗██║  ██║
	╚═╝     ╚══════╝ ╚═════╝    ╚═╝      ╚═╝   ╚══════╝╚═╝  ╚═╝    ╚══════╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝  ╚═╝╚═╝╚═╝  ╚═╝╚══════╝╚═╝  ╚═╝
	`)

	// Load configs from Environment Variables
	cfg := config.Load()

	// Initialize storage
	store := storage.NewFSBackend(cfg.DataDir)
	if err := store.Init(); err != nil {
		slog.Error("failed to init storage", "error", err)
		os.Exit(1)
	}

	// Initialize spec management (Parity file-based workflow)
	specStore := specs.NewStore(cfg.SpecsDir)
	if err := specStore.Init(); err != nil {
		slog.Error("failed to init spec storage", "error", err)
		os.Exit(1)
	}
	specService := specs.NewService(specStore, specs.Options{
		Generator: &specs.FlutterPubGenerator{},
		ServerURL: serverBaseURL(cfg.Port),
		Timeout:   cfg.SpecJobTimeout,
		MaxJobs:   cfg.MaxSpecJobs,
		MaxUpload: cfg.MaxSpecUploadBytes,
	})
	specService.Start()
	defer specService.Stop()

	mux := http.NewServeMux()

	// Validator
	validator := proxy.NewValidator(cfg.NoSumDB, cfg.AllowList, cfg.DenyList)

	// Handlers
	proxyHandler := proxy.NewProxyHandler(store, cfg.UpstreamProxy, validator)
	uploadHandler := upload.NewUploadHandler(store)
	if _, err := os.Stat("internal/dashboard/templates/index.html"); err != nil {
		log.Fatalf("dashboard template missing (run from the repository root): %v", err)
	}
	dashboardHandler := dashboard.NewDashboardHandler(store, specService)
	specHandler := specs.NewHandler(specService, cfg.MaxSpecUploadBytes)
	specHandler.Register(mux)

	// Routes
	mux.Handle("/upload", uploadHandler)

	// Intelligent routing: If path contains package API markers, send to proxy.
	// Otherwise, serve the dashboard.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Package requests contain /api/packages/ or /packages/
		if proxy.IsPackageRequest(r.URL.Path) {
			proxyHandler.ServeHTTP(w, r)
			return
		}

		// Default to dashboard
		dashboardHandler.ServeHTTP(w, r)
	})

	log.Printf("🚀 FlutterZakhireh is running on port: %s", cfg.Port)
	log.Printf("📊 Dashboard: http://localhost%s", cfg.Port)
	log.Printf("📦 Data directory: %s", cfg.DataDir)
	log.Printf("📄 Specs directory: %s", cfg.SpecsDir)
	log.Fatal(http.ListenAndServe(cfg.Port, server.AccessLogger(mux)))
}

// serverBaseURL derives the proxy's own base URL from the listen address so
// spec generation jobs can point PUB_HOSTED_URL back at this proxy.
func serverBaseURL(port string) string {
	addr := port
	if len(addr) > 0 && addr[0] == ':' {
		addr = "127.0.0.1" + addr
	}
	return "http://" + addr
}
