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
	// Ensure /logs directory exists
	logDir := "logs"
	if _, err := os.Stat(logDir); os.IsNotExist(err) {
		if err := os.Mkdir(logDir, 0755); err != nil {
			log.Fatalf("Failed to create log directory: %v", err)
		}
	}

	// Create log file with timestamp
	timestamp := time.Now().Format("2006-01-02_15-04-05")
	logFileName := filepath.Join(logDir, fmt.Sprintf("flutterzakhireh_%s.log", timestamp))
	logFile, err := os.OpenFile(logFileName, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0666)
	if err != nil {
		log.Fatalf("Failed to open log file: %v", err)
	}
	defer logFile.Close()

	// Set up multi-writer for logging to both terminal and log file
	multiWriter := io.MultiWriter(os.Stdout, logFile)
	log.SetOutput(multiWriter)

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
