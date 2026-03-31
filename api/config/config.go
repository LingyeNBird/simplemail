package config

import (
	"os"
	"strconv"
)

type Config struct {
	Port         string
	DBDSN        string
	RateLimit    int
	RateWindow   int    // seconds
	SMTPServerIP string // 仅从 SMTP_SERVER_IP 环境变量读取
	SMTPHostname string // 邮件服务器场指向的 hostname，不硬编码
}

func Load() *Config {
	rl, _ := strconv.Atoi(getEnv("RATE_LIMIT", "500"))
	rw, _ := strconv.Atoi(getEnv("RATE_WINDOW", "60"))

	return &Config{
		Port:         getEnv("PORT", "8081"),
		DBDSN:        getEnv("DB_DSN", ""),
		RateLimit:    rl,
		RateWindow:   rw,
		SMTPServerIP: os.Getenv("SMTP_SERVER_IP"),
		SMTPHostname: os.Getenv("SMTP_HOSTNAME"),
	}
}

func getEnv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
