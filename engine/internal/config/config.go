package config

import (
	"fmt"
	"os"
	"strconv"
)

// Config is runtime configuration for the Continuum engine node.
type Config struct {
	NodeID         string
	PostgresDSN    string
	S3Endpoint     string
	S3Region       string
	S3Bucket       string
	S3AccessKey    string
	S3SecretKey    string
	S3UsePathStyle bool
	DataDir        string
	ValkeyAddr     string
	ListenAddr     string
}

// LoadFromEnv reads configuration from environment variables.
func LoadFromEnv() (Config, error) {
	cfg := Config{
		NodeID:         envOr("CONTINUUM_NODE_ID", "node-1"),
		PostgresDSN:    envOr("CONTINUUM_POSTGRES_DSN", "postgres://continuum:continuum@localhost:5432/continuum?sslmode=disable"),
		S3Endpoint:     envOr("CONTINUUM_S3_ENDPOINT", "http://localhost:9000"),
		S3Region:       envOr("CONTINUUM_S3_REGION", "us-east-1"),
		S3Bucket:       envOr("CONTINUUM_S3_BUCKET", "continuum"),
		S3AccessKey:    envOr("CONTINUUM_S3_ACCESS_KEY", "minioadmin"),
		S3SecretKey:    envOr("CONTINUUM_S3_SECRET_KEY", "minioadmin"),
		S3UsePathStyle: envBool("CONTINUUM_S3_PATH_STYLE", true),
		DataDir:        envOr("CONTINUUM_DATA_DIR", "/var/lib/continuum"),
		ValkeyAddr:     envOr("CONTINUUM_VALKEY_ADDR", "localhost:6379"),
		ListenAddr:     envOr("CONTINUUM_LISTEN_ADDR", ":8080"),
	}
	if cfg.PostgresDSN == "" {
		return cfg, fmt.Errorf("CONTINUUM_POSTGRES_DSN is required")
	}
	if cfg.S3Bucket == "" {
		return cfg, fmt.Errorf("CONTINUUM_S3_BUCKET is required")
	}
	return cfg, nil
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
