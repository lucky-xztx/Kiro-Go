// Package main provides the entry point for Kiro API Proxy.
//
// Kiro API Proxy is a reverse proxy service that translates Kiro API requests
// into OpenAI and Anthropic (Claude) compatible formats. Key features include:
//   - Multi-account pool with round-robin load balancing
//   - Automatic OAuth token refresh
//   - Streaming response support for real-time AI interactions
//   - Admin panel for account and configuration management
//
// The service exposes the following endpoints:
//   - /v1/messages - Claude API compatible endpoint
//   - /v1/chat/completions - OpenAI API compatible endpoint
//   - /admin - Web-based administration panel
package main

import (
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"kiro-go/pool"
	"kiro-go/proxy"
	"kiro-go/store"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

func main() {
	// 配置文件路径，支持环境变量覆盖
	configPath := "data/config.json"
	if envPath := os.Getenv("CONFIG_PATH"); envPath != "" {
		configPath = envPath
	}

	// 确保数据目录存在
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		log.Fatalf("Failed to create data directory: %v", err)
	}

	// 加载配置
	if err := config.Init(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Initialize log level: LOG_LEVEL env var takes priority over config, defaulting to "info".
	logger.Init(config.GetLogLevel())

	// 环境变量覆盖密码
	if envPassword := os.Getenv("ADMIN_PASSWORD"); envPassword != "" {
		config.SetPassword(envPassword)
	}

	// 初始化用户/Key 数据库 (SQLite)
	dataDir := filepath.Dir(configPath)
	dbPath := filepath.Join(dataDir, "kiro.db")
	if envDB := os.Getenv("DB_PATH"); envDB != "" {
		dbPath = envDB
	}
	if err := store.Init(dbPath); err != nil {
		log.Fatalf("Failed to init store: %v", err)
	}
	// 确保至少有一个 admin（首启用 config.Password 创建）
	admin, err := store.EnsureAdmin(config.GetPassword())
	if err != nil {
		log.Fatalf("Failed to ensure admin user: %v", err)
	}
	logger.Infof("Admin user ready: %s", admin.Username)
	// 一次性把旧的 config.ApiKeys 迁移到 admin 名下
	if legacy := config.GetLegacyApiKeysForMigration(); len(legacy) > 0 {
		converted := make([]store.LegacyApiKey, 0, len(legacy))
		for _, e := range legacy {
			converted = append(converted, store.LegacyApiKey{
				ID:            e.ID,
				Name:          e.Name,
				Key:           e.Key,
				Enabled:       e.Enabled,
				CreatedAt:     e.CreatedAt,
				LastUsedAt:    e.LastUsedAt,
				TokenLimit:    e.TokenLimit,
				CreditLimit:   e.CreditLimit,
				TokensUsed:    e.TokensUsed,
				CreditsUsed:   e.CreditsUsed,
				RequestsCount: e.RequestsCount,
			})
		}
		if n, err := store.MigrateLegacyApiKeys(admin.ID, converted); err != nil {
			logger.Warnf("Migrate legacy api keys: %v", err)
		} else if n > 0 {
			logger.Infof("Migrated %d legacy api key(s) to admin", n)
			_ = config.ClearLegacyApiKeys()
		}
	}

	// 初始化账号池
	pool.GetPool()

	// 创建 HTTP 处理器（包含后台刷新任务）
	handler := proxy.NewHandler()

	// 启动账号健康检查循环：每 10 分钟探测一次。
	handler.StartHealthCheckLoop(10 * time.Minute)

	// 启动服务器
	addr := fmt.Sprintf("%s:%d", config.GetHost(), config.GetPort())
	logger.Infof("Kiro-Go starting on http://%s (log level: %s)", addr, logger.LevelName(logger.GetLevel()))
	logger.Infof("Admin panel: http://%s/admin", addr)
	logger.Infof("Claude API: http://%s/v1/messages", addr)
	logger.Infof("OpenAI API: http://%s/v1/chat/completions", addr)

	// WriteTimeout intentionally 0: SSE streams can run for minutes while the
	// upstream model produces tokens. ReadHeaderTimeout + ReadTimeout still
	// guard against slowloris-style header/body stalls.
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 30 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	if err := srv.ListenAndServe(); err != nil {
		logger.Fatalf("Server failed: %v", err)
	}
}
