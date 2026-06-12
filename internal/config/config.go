package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	ServerPort          string
	UpstreamAPI         string
	UpstreamToken       string
	DefaultMode         string
	DefaultModel        string
	UpstreamNativeTools bool
}

func Load() *Config {
	_ = godotenv.Load()
	_ = godotenv.Load(".env.local")

	cfg := &Config{
		ServerPort:          getEnv("SERVER_PORT", "8080"),
		UpstreamAPI:         getEnv("UPSTREAM_API", "https://api.example.com"),
		UpstreamToken:       getEnv("UPSTREAM_TOKEN", ""),
		DefaultMode:         getEnv("DEFAULT_MODE", "react"),
		DefaultModel:        getEnv("DEFAULT_MODEL", "deepseek/deepseek-chat-v3-0324"),
		UpstreamNativeTools: getEnvBool("UPSTREAM_NATIVE_TOOLS", false),
	}

	log.Printf("config loaded: SERVER_PORT=%s, UPSTREAM_API=%s, DEFAULT_MODE=%s, DEFAULT_MODEL=%s, UPSTREAM_NATIVE_TOOLS=%v",
		cfg.ServerPort, cfg.UpstreamAPI, cfg.DefaultMode, cfg.DefaultModel, cfg.UpstreamNativeTools)

	return cfg
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getEnvBool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes", "YES", "on", "ON":
		return true
	case "0", "false", "FALSE", "no", "NO", "off", "OFF":
		return false
	default:
		return fallback
	}
}
