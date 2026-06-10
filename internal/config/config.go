package config

import (
	"log"
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	ServerPort    string
	UpstreamAPI   string // 逆向 API 地址
	UpstreamToken string // 逆向 API 的 token（如果有）
	DefaultMode   string // "react" or "plan_execute"
}

func Load() *Config {
	// 加载 .env 文件（如果存在）
	_ = godotenv.Load()
	// 也尝试加载 .env.local（优先级更高）
	_ = godotenv.Load(".env.local")

	cfg := &Config{
		ServerPort:    getEnv("SERVER_PORT", "8080"),
		UpstreamAPI:   getEnv("UPSTREAM_API", "https://api.example.com"),
		UpstreamToken: getEnv("UPSTREAM_TOKEN", ""),
		DefaultMode:   getEnv("DEFAULT_MODE", "react"),
	}

	log.Printf("配置已加载: SERVER_PORT=%s, UPSTREAM_API=%s, DEFAULT_MODE=%s",
		cfg.ServerPort, cfg.UpstreamAPI, cfg.DefaultMode)

	return cfg
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
