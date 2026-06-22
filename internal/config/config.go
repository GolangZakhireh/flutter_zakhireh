package config

import (
	"os"
	"path/filepath"
)

// Config holds the application configuration
type Config struct {
	Port          string
	DataDir       string
	UpstreamProxy string
	NoSumDB       string
	AllowList     string
	DenyList      string
}

// Load loads the configuration from environment variables or defaults
func Load() *Config {
	return &Config{
		Port:          getEnv("FLUTTERZAKHIREH_PORT", ":8811"),
		DataDir:       getEnv("FLUTTERZAKHIREH_DATA_DIR", filepath.Join(".", "data", "packages")),
		UpstreamProxy: getEnv("FLUTTERZAKHIREH_UPSTREAM", "https://pub.dev"),
		NoSumDB:       getEnv("FLUTTER_NO_SUMDB", ""),
		AllowList:     getEnv("FLUTTERZAKHIREH_ALLOW", ""),
		DenyList:      getEnv("FLUTTERZAKHIREH_DENY", ""),
	}
}

func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
