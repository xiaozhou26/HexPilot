package main

import (
	"fmt"
	"log"

	"github.com/gin-gonic/gin"

	"github.com/hexpilot/api-proxy/internal/config"
	"github.com/hexpilot/api-proxy/internal/handler"
)

func main() {
	// 加载配置
	cfg := config.Load()

	// 打印启动信息
	fmt.Println("╔════════════════════════════════════════╗")
	fmt.Println("║      HexPilot API Proxy Server         ║")
	fmt.Println("╠════════════════════════════════════════╣")
	fmt.Printf("║  Server:     http://localhost:%s       ║\n", cfg.ServerPort)
	fmt.Printf("║  Upstream:   %s       ║\n", cfg.UpstreamAPI)
	fmt.Printf("║  Mode:       %-26s║\n", cfg.DefaultMode)
	fmt.Println("╠════════════════════════════════════════╣")
	fmt.Println("║  Endpoints:                            ║")
	fmt.Println("║    POST /v1/responses                  ║")
	fmt.Println("║    POST /v1/chat/completions           ║")
	fmt.Println("║    GET  /health                        ║")
	fmt.Println("╚════════════════════════════════════════╝")

	// 设置 Gin 模式
	gin.SetMode(gin.ReleaseMode)
	r := gin.Default()

	// 注册路由
	h := handler.New(cfg)
	h.RegisterRoutes(r)

	// 启动服务器
	addr := ":" + cfg.ServerPort
	if err := r.Run(addr); err != nil {
		log.Fatal("Server failed to start:", err)
	}
}
