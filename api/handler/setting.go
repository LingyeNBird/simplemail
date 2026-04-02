package handler

import (
	"bufio"
	"net/http"
	"os"
	"strings"

	"tempmail/store"

	"github.com/gin-gonic/gin"
)

var settingToEnv = map[string]string{
	"smtp_server_ip": "SMTP_SERVER_IP",
	"smtp_hostname":  "SMTP_HOSTNAME",
}

type SettingHandler struct {
	store       *store.Store
	domainH     *DomainHandler
	envFilePath string
}

func NewSettingHandler(s *store.Store, domainH *DomainHandler, envFilePath string) *SettingHandler {
	return &SettingHandler{store: s, domainH: domainH, envFilePath: envFilePath}
}

// GET /public/settings → 返回前端需要的公开配置
func (h *SettingHandler) GetPublic(c *gin.Context) {
	regOpen, err := h.store.GetSetting(c.Request.Context(), "registration_open")
	if err != nil {
		regOpen = "false"
	}
	siteTitle, _ := h.store.GetSetting(c.Request.Context(), "site_title")
	smtpIP, _ := h.store.GetSetting(c.Request.Context(), "smtp_server_ip")
	smtpHostname, _ := h.store.GetSetting(c.Request.Context(), "smtp_hostname")
	announce, _ := h.store.GetSetting(c.Request.Context(), "announcement")
	c.JSON(http.StatusOK, gin.H{
		"registration_open": regOpen == "true",
		"site_title":        siteTitle,
		"smtp_server_ip":    smtpIP,
		"smtp_hostname":     smtpHostname,
		"announcement":      announce,
	})
}

// GET /api/admin/settings → 读取所有设置（管理员）
func (h *SettingHandler) AdminGetAll(c *gin.Context) {
	settings, err := h.store.GetAllSettings(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, settings)
}

// PUT /api/admin/settings → 更新设置（管理员）
func (h *SettingHandler) AdminUpdate(c *gin.Context) {
	var req map[string]string
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	allowed := map[string]bool{
		"registration_open":      true,
		"rate_limit_enabled":     true,
		"max_mailboxes_per_user": true,
		"smtp_server_ip":         true,
		"smtp_hostname":          true,
		"site_title":             true,
		"announcement":           true,
		"default_domain":         true,
		"mailbox_ttl_minutes":    true,
		"cf_api_token":           true,
	}

	envUpdates := make(map[string]string)
	for k, v := range req {
		if !allowed[k] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown setting key: " + k})
			return
		}
		if err := h.store.SetSetting(c.Request.Context(), k, v); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		if envKey, ok := settingToEnv[k]; ok && os.Getenv(envKey) != "" {
			envUpdates[envKey] = v
		}
	}

	if h.domainH != nil {
		ip, _ := h.store.GetSetting(c.Request.Context(), "smtp_server_ip")
		hn, _ := h.store.GetSetting(c.Request.Context(), "smtp_hostname")
		h.domainH.UpdateConfig(ip, hn)
	}

	if h.envFilePath != "" && len(envUpdates) > 0 {
		h.writeEnvFile(envUpdates)
	}

	c.JSON(http.StatusOK, gin.H{"message": "settings updated"})
}

func (h *SettingHandler) writeEnvFile(updates map[string]string) {
	f, err := os.OpenFile(h.envFilePath, os.O_RDWR, 0644)
	if err != nil {
		return
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	updated := make(map[string]bool)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			lines = append(lines, line)
			continue
		}
		if eqIdx := strings.Index(line, "="); eqIdx > 0 {
			key := strings.TrimSpace(line[:eqIdx])
			if newVal, ok := updates[key]; ok {
				lines = append(lines, key+"="+newVal)
				updated[key] = true
				continue
			}
		}
		lines = append(lines, line)
	}
	f.Truncate(0)
	f.Seek(0, 0)
	w := bufio.NewWriter(f)
	for _, line := range lines {
		w.WriteString(line + "\n")
	}
	w.Flush()
}
