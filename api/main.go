package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"tempmail/config"
	"tempmail/handler"
	"tempmail/middleware"
	"tempmail/store"

	"github.com/gin-contrib/cors"
	"github.com/gin-gonic/gin"
)

func main() {
	cfg := config.Load()

	// ==================== 连接数据库 ====================
	ctx := context.Background()
	db, err := store.New(ctx, cfg.DBDSN)
	if err != nil {
		log.Fatalf("failed to connect database: %v", err)
	}
	defer db.Close()
	log.Println("✓ Database connected")

	// ==================== 启动时同步环境变量 → DB ====================
	if cfg.SMTPServerIP != "" {
		if dbIP, _ := db.GetSetting(ctx, "smtp_server_ip"); dbIP != cfg.SMTPServerIP {
			_ = db.SetSetting(ctx, "smtp_server_ip", cfg.SMTPServerIP)
			log.Printf("✓ Synced SMTP_SERVER_IP from env to DB: %s", cfg.SMTPServerIP)
		}
	}

	// ==================== Gin 路由 ====================
	gin.SetMode(gin.ReleaseMode)
	r := gin.New()
	r.Use(gin.Recovery())

	// CORS：允许前端跨域访问
	r.Use(cors.New(cors.Config{
		AllowOrigins:  []string{"*"},
		AllowMethods:  []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowHeaders:  []string{"Origin", "Content-Type", "Authorization"},
		ExposeHeaders: []string{"X-RateLimit-Limit", "X-RateLimit-Remaining", "X-RateLimit-Reset"},
		MaxAge:        12 * time.Hour,
	}))

	// 健康检查（无需认证）
	r.GET("/health", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok", "time": time.Now().Unix()})
	})

	// 初始化 handlers
	accountH := handler.NewAccountHandler(db)
	domainH := handler.NewDomainHandler(db, cfg.SMTPServerIP)
	mailboxH := handler.NewMailboxHandler(db)
	emailH := handler.NewEmailHandler(db)
	retainedMailH := handler.NewRetainedMailHandler(db)
	settingH := handler.NewSettingHandler(db, domainH, cfg.EnvFilePath)
	registerH := handler.NewRegisterHandler(db)
	statsH := handler.NewStatsHandler(db)

	// 公开路由（无需认证）
	public := r.Group("/public")
	{
		public.GET("/settings", settingH.GetPublic)
		public.POST("/register", registerH.Register)
		public.GET("/stats", statsH.Get)
	}

	// API 路由组（需要认证 + 速率限制）
	rl := middleware.NewInMemoryRateLimiter(cfg.RateLimit, cfg.RateWindow)
	api := r.Group("/api")
	api.Use(middleware.Auth(db))
	api.Use(middleware.RateLimit(rl))
	{
		// 当前用户
		api.GET("/me", accountH.Me)

		// 域名池（所有用户可查看）
		api.GET("/domains", domainH.List)
		api.GET("/domains/:id/status", domainH.GetStatus) // 任意用户可轮询域名状态
		api.GET("/stats", statsH.Get)
		// 任意已登录用户可提交域名进行 MX 自动验证
		api.POST("/domains/submit", domainH.Submit)

		// 邮箱管理
		api.POST("/mailboxes", mailboxH.Create)
		api.GET("/mailboxes", mailboxH.List)
		api.DELETE("/mailboxes/:id", mailboxH.Delete)
		api.PUT("/mailboxes/:id/renew", mailboxH.Renew)

		// 邮件管理
		api.GET("/mailboxes/:id/emails", emailH.List)
		api.GET("/mailboxes/:id/emails/:email_id", emailH.Get)
		api.DELETE("/mailboxes/:id/emails/:email_id", emailH.Delete)
		// 管理员路由
		admin := api.Group("/admin")
		admin.Use(middleware.AdminOnly())
		{
			admin.POST("/accounts", accountH.Create)
			admin.GET("/accounts", accountH.List)
			admin.DELETE("/accounts/:id", accountH.Delete)

			admin.POST("/domains", domainH.Add)
			admin.DELETE("/domains/:id", domainH.Delete)
			admin.PUT("/domains/:id/toggle", domainH.Toggle)
			admin.POST("/domains/mx-import", domainH.MXImport)
			admin.POST("/domains/mx-register", domainH.MXRegister)
			admin.POST("/domains/cf-create", domainH.CFCreate)
			admin.DELETE("/domains/:id/cf", domainH.CFDelete)
			admin.PUT("/domains/:id/hostname", domainH.UpdateHostname)
			admin.PUT("/domains/batch/toggle", domainH.BatchToggle)
			admin.PUT("/domains/batch/delete", domainH.BatchDelete)
			admin.PUT("/domains/batch/cf-delete", domainH.BatchCFDelete)
			admin.GET("/domains/:id/status", domainH.GetStatus)

			// 系统设置管理
			admin.GET("/settings", settingH.AdminGetAll)
			admin.PUT("/settings", settingH.AdminUpdate)
			admin.GET("/retained-mails", retainedMailH.List)
			admin.GET("/retained-mails/:id", retainedMailH.Get)
			admin.DELETE("/retained-mails/:id", retainedMailH.Delete)
		}
	}

	// 内部邮件投递接口（Postfix pipe 调用，仅限本机访问）
	internal := r.Group("/internal")
	internal.Use(func(c *gin.Context) {
		if c.ClientIP() != "127.0.0.1" && c.ClientIP() != "::1" {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{"error": "internal endpoint"})
			return
		}
		c.Next()
	})
	{
		// 域名列表（供 Postfix 同步）
		internal.GET("/domains", func(c *gin.Context) {
			domains, err := db.ListDomains(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}
			c.JSON(http.StatusOK, gin.H{"domains": domains})
		})

		// 返回应该写入 Postfix virtual_domains 的条目
		// 分两类：
		//   - hash_domains: is_active=1 的域名（原有白名单，写 hash 表，精确匹配）
		//   - regexp_lines: 由 retained_domain_patterns 生成的 regexp 表条目
		//                  （补充收信面，写 regexp 表，后缀/通配符匹配）
		// Postfix 端 virtual_mailbox_domains = hash:..., regexp:... 并联，
		// 任一命中即收信。
		internal.GET("/virtual-domains", func(c *gin.Context) {
			domains, err := db.ListDomains(c.Request.Context())
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			activeDomains := make([]string, 0, len(domains))
			for _, d := range domains {
				if d.IsActive {
					activeDomains = append(activeDomains, d.Domain)
				}
			}

			// 读取管理员配置的留存域名匹配模式
			patternsJSON, err := db.GetSetting(c.Request.Context(), "retained_domain_patterns")
			if err != nil || strings.TrimSpace(patternsJSON) == "" {
				patternsJSON = "[]"
			}
			var patterns []string
			if err := json.Unmarshal([]byte(patternsJSON), &patterns); err != nil {
				log.Printf("[virtual-domains] invalid retained_domain_patterns JSON: %v", err)
				patterns = nil
			}

			regexpLines := buildRetainedRegexpLines(patterns)

			c.JSON(http.StatusOK, gin.H{
				"hash_domains": activeDomains,
				"regexp_lines": regexpLines,
			})
		})

		internal.POST("/deliver", func(c *gin.Context) {
			var req struct {
				Recipient string `json:"recipient" binding:"required"`
				Sender    string `json:"sender"`
				Subject   string `json:"subject"`
				BodyText  string `json:"body_text"`
				BodyHTML  string `json:"body_html"`
				Raw       string `json:"raw"`
			}
			if err := c.ShouldBindJSON(&req); err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}

			// 查找收件邮箱
			mailbox, err := db.GetMailboxByFullAddress(c.Request.Context(), req.Recipient)
			if err != nil {
				if err != sql.ErrNoRows {
					c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
					return
				}

				retainedMail, retainErr := db.InsertRetainedMail(
					c.Request.Context(),
					req.Recipient,
					req.Sender,
					req.Subject,
					req.BodyText,
					req.BodyHTML,
					req.Raw,
				)
				if retainErr != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": retainErr.Error()})
					return
				}

				c.JSON(http.StatusOK, gin.H{"status": "retained", "retained_mail_id": retainedMail.ID})
				return
			}

			// 存储邮件
			email, err := db.InsertEmail(c.Request.Context(),
				mailbox.ID, req.Sender, req.Subject, req.BodyText, req.BodyHTML, req.Raw)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
				return
			}

			c.JSON(http.StatusOK, gin.H{"status": "delivered", "email_id": email.ID})
		})
	}

	// ==================== 邮箱自动过期清理 ====================
	go func() {
		ticker := time.NewTicker(1 * time.Minute)
		defer ticker.Stop()
		log.Println("✓ Mailbox expiry cleaner started (TTL=30min, interval=1min)")
		for range ticker.C {
			if deleted, err := db.DeleteExpiredMailboxes(context.Background()); err != nil {
				log.Printf("[cleaner] error: %v", err)
			} else if deleted > 0 {
				log.Printf("[cleaner] deleted %d expired mailboxes", deleted)
			}
		}
	}()

	// ==================== MX 自动验证轮询 ====================
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		log.Println("✓ MX domain verifier started (pending check=30s, active re-check=6h)")
		reCheckTicker := time.NewTicker(6 * time.Hour)
		defer reCheckTicker.Stop()
		for {
			select {
			case <-ticker.C:
				// 处理待验证域名
				pendingDomains, err := db.ListPendingDomains(context.Background())
				if err != nil {
					log.Printf("[mx-verifier] list pending error: %v", err)
					continue
				}
				if len(pendingDomains) == 0 {
					continue
				}
				serverIP := domainH.GetServerIP()
				for _, d := range pendingDomains {
					matched, _, mxStatus := store.CheckDomainMX(d.Domain, serverIP)
					db.TouchDomainCheckTime(context.Background(), d.ID)
					if matched {
						if err := db.PromoteDomainToActive(context.Background(), d.ID); err != nil {
							log.Printf("[mx-verifier] promote %s error: %v", d.Domain, err)
						} else {
							log.Printf("[mx-verifier] ✓ %s MX verified, domain activated", d.Domain)
						}
					} else {
						log.Printf("[mx-verifier] waiting: %s — %s", d.Domain, mxStatus)
					}
				}

			case <-reCheckTicker.C:
				// 每 6 小时重新检测所有已激活域名，MX 失效则自动停用
				activeDomains, err := db.GetActiveDomains(context.Background())
				if err != nil {
					log.Printf("[mx-recheck] list active error: %v", err)
					continue
				}
				serverIP := domainH.GetServerIP()
				log.Printf("[mx-recheck] checking %d active domains", len(activeDomains))
				for _, d := range activeDomains {
					matched, _, mxStatus := store.CheckDomainMX(d.Domain, serverIP)
					db.TouchDomainCheckTime(context.Background(), d.ID)
					if !matched {
						if err := db.DisableDomainMX(context.Background(), d.ID); err != nil {
							log.Printf("[mx-recheck] disable %s error: %v", d.Domain, err)
						} else {
							log.Printf("[mx-recheck] ⚠ %s MX no longer valid (%s), domain disabled", d.Domain, mxStatus)
						}
					}
				}
			}
		}
	}()

	// ==================== 写出管理员 API Key 文件 ====================
	go func() {
		// 等待 DB 就绪后再读取（延迟 1 秒）
		time.Sleep(1 * time.Second)
		adminKey, err := db.GetAdminAPIKey(context.Background())
		if err != nil {
			log.Printf("[adminkey] could not fetch admin key: %v", err)
			return
		}
		keyFile := os.Getenv("ADMIN_KEY_FILE")
		if keyFile == "" {
			keyFile = "/data/admin.key"
		}
		if err := os.MkdirAll(filepath.Dir(keyFile), 0700); err == nil {
			content := "# TempMail Admin API Key\n# Auto-generated on startup — keep this secret!\n\nADMIN_API_KEY=" + adminKey + "\n"
			if err := os.WriteFile(keyFile, []byte(content), 0600); err != nil {
				log.Printf("[adminkey] write file error: %v", err)
			} else {
				log.Printf("✓ Admin API Key written to %s", keyFile)
			}
		}
		log.Printf("✴ ADMIN API KEY: %s", adminKey)
	}()

	// ==================== 启动服务 ====================
	srv := &http.Server{
		Addr:         "127.0.0.1:" + cfg.Port,
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	go func() {
		log.Printf("✓ API server listening on :%s", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("listen: %v", err)
		}
	}()

	// 优雅关闭
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit
	log.Println("Shutting down server...")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Fatalf("Server forced to shutdown: %v", err)
	}
	log.Println("Server exited")
}

// buildRetainedRegexpLines 把用户配置的 retained_domain_patterns 转换成
// Postfix regexp: 表条目。格式：每行 "<正则> <结果>"。
// 支持的 pattern 语法：
//   - "*"                → 匹配所有域名
//   - "*.example.com"    → 匹配 example.com 本身及其任意子域
//   - "example.com"      → 仅精确匹配 example.com
// 其它字符按字面处理（仅做正则元字符转义，不做语义扩展）。
func buildRetainedRegexpLines(patterns []string) []string {
	if len(patterns) == 0 {
		return nil
	}
	lines := make([]string, 0, len(patterns))
	for _, p := range patterns {
		p = strings.ToLower(strings.TrimSpace(p))
		if p == "" {
			continue
		}
		var re string
		switch {
		case p == "*":
			// 全收——正则匹配任何非空域名
			re = `/.+/`
		case strings.HasPrefix(p, "*."):
			// 子域通配：*.example.com → 匹配 example.com 及任意前置子域
			suffix := regexpEscape(strings.TrimPrefix(p, "*."))
			if suffix == "" {
				continue
			}
			re = `/(^|\.)` + suffix + `$/`
		default:
			// 精确匹配
			re = `/^` + regexpEscape(p) + `$/`
		}
		lines = append(lines, re+" OK")
	}
	return lines
}

// regexpEscape 转义正则元字符，把域名当字面量处理。
func regexpEscape(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '.', '+', '*', '?', '(', ')', '[', ']', '{', '}', '^', '$', '|', '\\':
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}
