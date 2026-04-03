package handler

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"tempmail/cf"
	"tempmail/middleware"
	"tempmail/store"

	"github.com/gin-gonic/gin"
)

type DomainHandler struct {
	store       *store.Store
	cfgIP       string // SMTP_SERVER_IP env
	cfgHostname string // SMTP_HOSTNAME env
}

func NewDomainHandler(s *store.Store, smtpIP, smtpHostname string) *DomainHandler {
	return &DomainHandler{store: s, cfgIP: smtpIP, cfgHostname: smtpHostname}
}

func (h *DomainHandler) getServerIP(ctx context.Context) string {
	if ip, err := h.store.GetSetting(ctx, "smtp_server_ip"); err == nil && ip != "" {
		return ip
	}
	return h.cfgIP
}

func (h *DomainHandler) GetServerIP() string {
	return h.getServerIP(context.Background())
}

func (h *DomainHandler) UpdateConfig(serverIP, hostname string) {
	h.cfgIP = serverIP
	h.cfgHostname = hostname
}

// getServerHostname 返回 MX 记录应指向的邮件服务器 hostname
// 优先: DB 设置 smtp_hostname → 环境变量 → 空串（傻用 mail.提交域名 方式）
func (h *DomainHandler) getServerHostname(ctx context.Context) string {
	if hn, err := h.store.GetSetting(ctx, "smtp_hostname"); err == nil && hn != "" {
		return hn
	}
	return h.cfgHostname
}

// POST /api/admin/domains - 添加域名到池（管理员）
func (h *DomainHandler) Add(c *gin.Context) {
	var req struct {
		Domain string `json:"domain" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	domain, err := h.store.AddDomain(c.Request.Context(), req.Domain)
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "domain already exists: " + err.Error()})
		return
	}

	// 获取服务器 IP 和 hostname（来自 DB 设置或环境变量）
	serverIP := h.getServerIP(c.Request.Context())
	hostname := h.getServerHostname(c.Request.Context())

	// 构建 DNS 指引
	var dnsRecords []gin.H
	if hostname != "" {
		dnsRecords = []gin.H{
			{"type": "MX", "host": "@", "value": hostname, "priority": 10, "description": "邮件交换记录，指向本服务器"},
			{"type": "TXT", "host": "@", "value": fmt.Sprintf("v=spf1 ip4:%s ~all", serverIP), "description": "SPF 记录（可选）"},
		}
	} else {
		dnsRecords = []gin.H{
			{"type": "MX", "host": "@", "value": fmt.Sprintf("mail.%s", req.Domain), "priority": 10, "description": "邮件交换记录"},
			{"type": "A", "host": fmt.Sprintf("mail.%s", req.Domain), "value": serverIP, "description": "邮件服务器 A 记录"},
			{"type": "TXT", "host": "@", "value": fmt.Sprintf("v=spf1 ip4:%s ~all", serverIP), "description": "SPF 记录（可选）"},
		}
	}

	// 返回 DNS 配置指引
	c.JSON(http.StatusCreated, gin.H{
		"domain":      domain,
		"dns_records": dnsRecords,
		"instructions": fmt.Sprintf(
			"请在域名 %s 的 DNS 管理面板中添加以上记录。添加后约 5-30 分钟生效。",
			req.Domain),
	})
}

// GET /api/domains - 列出所有域名（共享域名池）
func (h *DomainHandler) List(c *gin.Context) {
	_ = middleware.GetAccount(c) // 确保已认证

	domains, err := h.store.ListDomains(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"domains": domains})
}

// DELETE /api/admin/domains/:id - 删除域名（管理员）
func (h *DomainHandler) Delete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid domain id"})
		return
	}

	if err := h.store.DeleteDomain(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "domain deleted"})
}

// PUT /api/admin/domains/:id/toggle - 启用/禁用域名（管理员）
func (h *DomainHandler) Toggle(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid domain id"})
		return
	}

	var req struct {
		Active bool `json:"active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if err := h.store.ToggleDomain(c.Request.Context(), id, req.Active); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "domain updated"})
}

// POST /api/admin/domains/mx-import - MX快捷接入（DNS检测并自动导入）
// body: {"domain":"example.com", "force":false}
func (h *DomainHandler) MXImport(c *gin.Context) {
	var req struct {
		Domain string `json:"domain" binding:"required"`
		Force  bool   `json:"force"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))

	// 获取服务器 IP / hostname（来自 DB 设置或环境变量，不内置硬编码）
	serverIP := h.getServerIP(c.Request.Context())
	hostname := h.getServerHostname(c.Request.Context())

	// DNS MX 检测
	matched, mxHosts, mxStatus := store.CheckDomainMX(req.Domain, serverIP)

	if !matched && !req.Force {
		var dnsHint []gin.H
		if hostname != "" {
			dnsHint = []gin.H{
				{"type": "MX", "host": "@", "value": hostname, "priority": 10},
				{"type": "TXT", "host": "@", "value": fmt.Sprintf("v=spf1 ip4:%s ~all", serverIP)},
			}
		} else {
			mailSub := fmt.Sprintf("mail.%s", req.Domain)
			dnsHint = []gin.H{
				{"type": "MX", "host": "@", "value": mailSub, "priority": 10},
				{"type": "A", "host": mailSub, "value": serverIP},
				{"type": "TXT", "host": "@", "value": fmt.Sprintf("v=spf1 ip4:%s ~all", serverIP)},
			}
		}
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error":     "MX检测未通过，如确定要导入请加 force:true",
			"mx_status": mxStatus,
			"mx_hosts":  mxHosts,
			"server_ip": serverIP,
			"domain":    req.Domain,
			"dns_hint":  dnsHint,
		})
		return
	}

	// 导入到域名池
	domain, err := h.store.AddDomain(c.Request.Context(), req.Domain)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "域名已存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"domain":     domain,
		"mx_status":  mxStatus,
		"mx_matched": matched,
		"message":    fmt.Sprintf("域名 %s 已导入域名池，Postfix 将在 60 秒内自动同步", req.Domain),
	})
}

// POST /api/admin/domains/mx-register - 提交域名等待自动MX验证（无需手动确认）
// body: {"domain":"example.com"}
func (h *DomainHandler) MXRegister(c *gin.Context) {
	var req struct {
		Domain string `json:"domain" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))

	serverIP := h.getServerIP(c.Request.Context())
	hostname := h.getServerHostname(c.Request.Context())

	// MX 目标: 优先用服务器自己的 hostname，否则用用户域名的 mail 子域
	mxTarget := fmt.Sprintf("mail.%s", req.Domain)
	dnsRequired := []gin.H{
		{"type": "MX", "host": "@", "value": mxTarget, "priority": 10},
		{"type": "A", "host": mxTarget, "value": serverIP},
		{"type": "TXT", "host": "@", "value": fmt.Sprintf("v=spf1 ip4:%s ~all", serverIP)},
	}
	if hostname != "" {
		mxTarget = hostname
		dnsRequired = []gin.H{
			{"type": "MX", "host": "@", "value": hostname, "priority": 10},
			{"type": "TXT", "host": "@", "value": fmt.Sprintf("v=spf1 ip4:%s ~all", serverIP)},
		}
	}

	// 先尝试立即检测；通过则直接激活
	matched, _, mxStatus := store.CheckDomainMX(req.Domain, serverIP)
	if matched {
		domain, err := h.store.AddDomain(c.Request.Context(), req.Domain)
		if err != nil {
			if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
				// 已存在则直接返回
				domains, _ := h.store.ListDomains(c.Request.Context())
				for _, d := range domains {
					if d.Domain == req.Domain {
						c.JSON(http.StatusOK, gin.H{
							"domain":    d,
							"status":    d.Status,
							"mx_status": mxStatus,
							"message":   "域名已存在且处于激活状态",
						})
						return
					}
				}
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}
		c.JSON(http.StatusCreated, gin.H{
			"domain":  domain,
			"status":  "active",
			"message": "MX验证通过，域名已立即加入域名池",
		})
		return
	}

	// MX未通过 → 加入 pending，等待后台自动轮询
	domain, err := h.store.AddDomainPending(c.Request.Context(), req.Domain)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"domain":       domain,
		"status":       domain.Status,
		"server_ip":    serverIP,
		"mx_status":    mxStatus,
		"message":      fmt.Sprintf("域名 %s 已进入待验证队列，后台每30秒自动检测MX记录，通过后自动加入域名池", req.Domain),
		"dns_required": dnsRequired,
	})
}

// POST /api/domains/submit — 任意已登录用户提交域名进行 MX 自动验证
// 与 MXRegister 逻辑相同，但不需要管理员权限
func (h *DomainHandler) Submit(c *gin.Context) {
	h.MXRegister(c) // 复用相同逻辑
}

// GET /api/admin/domains/:id/status - 查询域名状态（用于前端轮询）
func (h *DomainHandler) GetStatus(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid domain id"})
		return
	}

	domain, err := h.store.GetDomainByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "domain not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":            domain.ID,
		"domain":        domain.Domain,
		"status":        domain.Status,
		"is_active":     domain.IsActive,
		"mx_checked_at": domain.MxCheckedAt,
	})
}

// POST /api/admin/domains/cf-create — 通过 Cloudflare API 自动创建子域名 MX 解析并加入域名池
//
// 请求示例: {"domain":"vet.nightunderfly.online"}
//
// 完整流程:
//  1. 校验系统设置中已配置 cf_api_token（需要 Zone:DNS:Edit 权限的 CF API Token）
//  2. 校验域名格式：至少包含两段（子域名.主域名，如 vet.nightunderfly.online）
//  3. 读取 smtp_hostname 作为 MX 记录的目标值（如 mail.nightunderfly.online）
//  4. 调用 CF API 根据"主域名"部分查找对应的 Zone ID
//  5. 调用 CF API 在该 Zone 下创建 MX 记录（subdomain → smtp_hostname）
//  6. 将域名以 pending 状态写入本地域名池，等待后台 MX 验证通过后自动激活
//
// 前置条件:
//   - 系统设置中已配置 cf_api_token（Cloudflare API Token，需 Zone:DNS:Edit 权限）
//   - 系统设置中已配置 smtp_hostname（邮件服务器主机名，作为 MX 记录目标）
//   - 输入的域名必须使用 Cloudflare 托管的 DNS
//
// 错误码:
//
//	400 — CF Token 未配置 / 域名格式不合法 / smtp_hostname 未配置 / Zone 未找到
//	409 — 域名已存在于本地域名池
//	502 — CF API 创建 DNS 记录失败
func (h *DomainHandler) CFCreate(c *gin.Context) {
	var req struct {
		Domain string `json:"domain" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))

	// 检查 CF API Token 是否已在系统设置中配置
	cfToken, err := h.store.GetSetting(c.Request.Context(), "cf_api_token")
	if err != nil || cfToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "未配置 Cloudflare API Token，请在系统设置中添加 cf_api_token（需要 DNS 编辑权限）",
		})
		return
	}

	// 校验域名至少包含两段（子域名 + 主域名），例如 vet.nightunderfly.online
	// 单段域名（如 nightunderfly.online）不适合通过此接口创建，应使用手动 MX 配置
	parts := strings.Split(req.Domain, ".")
	if len(parts) < 3 {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "域名至少需要两段，如 vet.nightunderfly.online（子域名 + 主域名）",
		})
		return
	}

	// 读取邮件服务器主机名作为 MX 记录指向的目标（如 mail.nightunderfly.online）
	hostname := h.getServerHostname(c.Request.Context())
	if hostname == "" {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": "未配置邮件服务器主机名（smtp_hostname），请在系统设置中添加",
		})
		return
	}

	// 通过 CF API 查找域名对应的主域名 Zone
	// 例如 vet.nightunderfly.online → 查找 nightunderfly.online 的 Zone ID
	client := cf.NewClient(cfToken)
	zone, err := client.FindZone(req.Domain)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error":  "查找 Cloudflare Zone 失败: " + err.Error(),
			"domain": req.Domain,
		})
		return
	}

	// 在找到的 Zone 下创建 MX 记录
	// subdomain 是相对 Zone 的子域名部分，如 "vet"（从 vet.nightunderfly.online 去掉 .nightunderfly.online）
	// MX 记录内容为 smtp_hostname，优先级 10，DNS only（不经过 CF 代理）
	subdomain := strings.TrimSuffix(req.Domain, "."+zone.Name)
	created, err := client.CreateMXRecord(zone.ID, subdomain, hostname)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error":     "创建 Cloudflare DNS 记录失败: " + err.Error(),
			"zone":      zone.Name,
			"subdomain": subdomain,
			"mx_target": hostname,
		})
		return
	}

	// 将域名以 pending 状态加入本地域名池
	// 后台 MX 验证器会每 30 秒检测 MX 记录，DNS 生效后自动激活
	domain, err := h.store.AddDomainPending(c.Request.Context(), req.Domain)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") || strings.Contains(err.Error(), "unique") {
			c.JSON(http.StatusConflict, gin.H{"error": "域名已存在"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"domain":    domain,
		"cf_record": created,
		"zone":      zone.Name,
		"mx_target": hostname,
		"message": fmt.Sprintf(
			"已在 Cloudflare Zone %s 中为 %s 创建 MX 记录（→ %s），域名已加入验证队列",
			zone.Name, req.Domain, hostname,
		),
	})
}

// DELETE /api/admin/domains/:id/cf — 通过 Cloudflare API 删除 MX 记录并从本地域名池移除
func (h *DomainHandler) CFDelete(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid domain id"})
		return
	}

	domain, err := h.store.GetDomainByID(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "domain not found"})
		return
	}

	cfToken, err := h.store.GetSetting(c.Request.Context(), "cf_api_token")
	if err != nil || cfToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未配置 Cloudflare API Token，请在系统设置中添加 cf_api_token"})
		return
	}

	hostname := h.getServerHostname(c.Request.Context())

	client := cf.NewClient(cfToken)
	zone, err := client.FindZone(domain.Domain)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "查找 Cloudflare Zone 失败: " + err.Error(), "domain": domain.Domain})
		return
	}

	subdomain := strings.TrimSuffix(domain.Domain, "."+zone.Name)
	record, err := client.FindMXRecord(zone.ID, subdomain, hostname)
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "查找 MX 记录失败: " + err.Error(), "zone": zone.Name, "subdomain": subdomain})
		return
	}

	deletedCF := false
	if record != nil {
		if err := client.DeleteDNSRecord(zone.ID, record.ID); err != nil {
			c.JSON(http.StatusBadGateway, gin.H{"error": "删除 Cloudflare DNS 记录失败: " + err.Error(), "record_id": record.ID})
			return
		}
		deletedCF = true
	}

	if err := h.store.DeleteDomain(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "删除本地域名失败: " + err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"message":           "域名已删除",
		"domain":            domain.Domain,
		"zone":              zone.Name,
		"cf_record_deleted": deletedCF,
	})
}

// PUT /api/admin/domains/batch/toggle — 批量启用/禁用域名
func (h *DomainHandler) BatchToggle(c *gin.Context) {
	var req struct {
		IDs    []int `json:"ids" binding:"required"`
		Active bool  `json:"active"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	updated := 0
	for _, id := range req.IDs {
		if err := h.store.ToggleDomain(c.Request.Context(), id, req.Active); err == nil {
			updated++
		}
	}

	c.JSON(http.StatusOK, gin.H{"updated": updated, "total": len(req.IDs)})
}

// PUT /api/admin/domains/batch/delete — 批量删除域名（仅本地）
func (h *DomainHandler) BatchDelete(c *gin.Context) {
	var req struct {
		IDs []int `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	deleted := 0
	for _, id := range req.IDs {
		if err := h.store.DeleteDomain(c.Request.Context(), id); err == nil {
			deleted++
		}
	}

	c.JSON(http.StatusOK, gin.H{"deleted": deleted, "total": len(req.IDs)})
}

// PUT /api/admin/domains/batch/cf-delete — 批量通过 CF API 删除 MX 记录并移除域名
func (h *DomainHandler) BatchCFDelete(c *gin.Context) {
	var req struct {
		IDs []int `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	cfToken, err := h.store.GetSetting(c.Request.Context(), "cf_api_token")
	if err != nil || cfToken == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未配置 Cloudflare API Token"})
		return
	}

	hostname := h.getServerHostname(c.Request.Context())
	client := cf.NewClient(cfToken)

	type batchResult struct {
		ID     int    `json:"id"`
		Domain string `json:"domain"`
		Status string `json:"status"`
		Error  string `json:"error,omitempty"`
	}

	var results []batchResult
	for _, id := range req.IDs {
		domain, err := h.store.GetDomainByID(c.Request.Context(), id)
		if err != nil {
			results = append(results, batchResult{ID: id, Status: "error", Error: "domain not found"})
			continue
		}

		zone, zoneErr := client.FindZone(domain.Domain)
		if zoneErr != nil {
			_ = h.store.DeleteDomain(c.Request.Context(), id)
			results = append(results, batchResult{ID: id, Domain: domain.Domain, Status: "deleted_local", Error: "zone not found: " + zoneErr.Error()})
			continue
		}

		subdomain := strings.TrimSuffix(domain.Domain, "."+zone.Name)
		record, findErr := client.FindMXRecord(zone.ID, subdomain, hostname)
		if findErr != nil {
			_ = h.store.DeleteDomain(c.Request.Context(), id)
			results = append(results, batchResult{ID: id, Domain: domain.Domain, Status: "deleted_local", Error: "find MX failed: " + findErr.Error()})
			continue
		}

		if record != nil {
			if delErr := client.DeleteDNSRecord(zone.ID, record.ID); delErr != nil {
				results = append(results, batchResult{ID: id, Domain: domain.Domain, Status: "error", Error: "delete CF record failed: " + delErr.Error()})
				continue
			}
		}

		if err := h.store.DeleteDomain(c.Request.Context(), id); err != nil {
			results = append(results, batchResult{ID: id, Domain: domain.Domain, Status: "error", Error: "delete local failed: " + err.Error()})
			continue
		}

		results = append(results, batchResult{ID: id, Domain: domain.Domain, Status: "success"})
	}

	c.JSON(http.StatusOK, gin.H{"results": results, "total": len(req.IDs)})
}
