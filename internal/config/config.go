package config

import (
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Config holds the application configuration
type Config struct {
	Port          string
	DataDir       string
	UpstreamProxy string
	NoSumDB       string
	AllowList     string
	DenyList      string

	// SpecsDir is the persistent root for uploaded POP Spec files and
	// their managed artifacts (workspace, outputs, logs, metadata).
	SpecsDir string
	// MaxSpecUploadBytes caps the size of an uploaded spec file.
	MaxSpecUploadBytes int64
	// MaxSpecJobs limits how many spec generation jobs run concurrently.
	MaxSpecJobs int
	// SpecJobTimeout bounds a single generation run.
	SpecJobTimeout time.Duration
	// FlutterBin is the binary used for dependency resolution
	// (flutter preferred, dart as fallback). Empty means auto-detect.
	FlutterBin string
}

// Load loads the configuration from environment variables or defaults
func Load() *Config {
	return &Config{
		Port:               getEnv("FLUTTERZAKHIREH_PORT", ":8811"),
		DataDir:            getEnv("FLUTTERZAKHIREH_DATA_DIR", filepath.Join(".", "data", "packages")),
		UpstreamProxy:      getEnv("FLUTTERZAKHIREH_UPSTREAM", "https://pub.dev"),
		NoSumDB:            getEnv("FLUTTER_NO_SUMDB", ""),
		AllowList:          getEnv("FLUTTERZAKHIREH_ALLOW", ""),
		DenyList:           getEnv("FLUTTERZAKHIREH_DENY", ""),
		SpecsDir:           getEnv("FLUTTERZAKHIREH_SPECS_DIR", filepath.Join(".", "data", "specs")),
		MaxSpecUploadBytes: getEnvInt64("FLUTTERZAKHIREH_SPEC_MAX_UPLOAD_BYTES", 1<<20),
		MaxSpecJobs:        getEnvInt("FLUTTERZAKHIREH_MAX_JOBS", 2),
		SpecJobTimeout:     getEnvDuration("FLUTTERZAKHIREH_JOB_TIMEOUT", 10*time.Minute),
		FlutterBin:         os.Getenv("FLUTTERZAKHIREH_FLUTTER_BIN"),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func getEnvInt(key string, fallback int) int {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func getEnvInt64(key string, fallback int64) int64 {
	if v, ok := os.LookupEnv(key); ok {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return fallback
}

func getEnvDuration(key string, fallback time.Duration) time.Duration {
	if v, ok := os.LookupEnv(key); ok {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return fallback
}
