package handler

import (
	"net/http"
	"strconv"

	"tempmail/store"

	"github.com/gin-gonic/gin"
)

type RetainedMailHandler struct {
	store *store.Store
}

func NewRetainedMailHandler(s *store.Store) *RetainedMailHandler {
	return &RetainedMailHandler{store: s}
}

// GET /api/admin/retained-mails - 列出保留邮件（管理员）
func (h *RetainedMailHandler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}

	mails, total, err := h.store.ListRetainedMails(c.Request.Context(), page, size)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"data":  mails,
		"total": total,
		"page":  page,
		"size":  size,
	})
}

// GET /api/admin/retained-mails/:id - 查看保留邮件详情（管理员）
func (h *RetainedMailHandler) Get(c *gin.Context) {
	id, err := parseUUID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retained mail id"})
		return
	}

	mail, err := h.store.GetRetainedMail(c.Request.Context(), id)
	if err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "retained mail not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"retained_mail": mail})
}

// DELETE /api/admin/retained-mails/:id - 删除保留邮件（管理员）
func (h *RetainedMailHandler) Delete(c *gin.Context) {
	id, err := parseUUID(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid retained mail id"})
		return
	}

	if err := h.store.DeleteRetainedMail(c.Request.Context(), id); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "retained mail not found"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "retained mail deleted"})
}
