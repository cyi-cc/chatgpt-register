package handlers

import (
	"net/http"
	"strings"

	"chatgpt-register/internal/models"
	"chatgpt-register/internal/varymail"

	"github.com/gin-gonic/gin"
)

// setting 读取单个系统设置值（不存在返回空串）。
func (h *Handler) setting(key string) string {
	var s models.Setting
	if err := h.DB.Where("key = ?", key).First(&s).Error; err != nil {
		return ""
	}
	return strings.TrimSpace(s.Value)
}

// VarymailServices 查询固定的 chatgpt 服务库存，供设置页验证 Key/看库存。
// 支持用 query 里的 key 临时覆盖（保存前先测试）。
func (h *Handler) VarymailServices(c *gin.Context) {
	key := strings.TrimSpace(c.Query("key"))
	if key == "" {
		key = h.setting("varymail_api_key")
	}
	if key == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请先填写 varymail API Key"})
		return
	}

	cli := varymail.New("", key)
	svc, price, err := cli.ServiceByName(c.Request.Context(), varymail.DefaultServiceName)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"service": svc, "price": price})
}
