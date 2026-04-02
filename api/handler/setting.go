package handler

import (
	"net/http"

	"tempmail/store"

	"github.com/gin-gonic/gin"
)

type SettingHandler struct {
	store   *store.Store
	domainH *DomainHandler
}

func NewSettingHandler(s *store.Store, domainH *DomainHandler) *SettingHandler {
	return &SettingHandler{store: s, domainH: domainH}
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

	// 白名单：已知配置项
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

	for k, v := range req {
		if !allowed[k] {
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown setting key: " + k})
			return
		}
		if err := h.store.SetSetting(c.Request.Context(), k, v); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
	}
	if h.domainH != nil {
		ip, _ := h.store.GetSetting(c.Request.Context(), "smtp_server_ip")
		hn, _ := h.store.GetSetting(c.Request.Context(), "smtp_hostname")
		h.domainH.UpdateConfig(ip, hn)
	}
	c.JSON(http.StatusOK, gin.H{"message": "settings updated"})
}
