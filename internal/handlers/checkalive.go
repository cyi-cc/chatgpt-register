package handlers

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"chatgpt-register/internal/models"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const usageLimitsURL = "https://chatgpt.com/backend-api/pageConfigs/usage_limits"

// checkAliveUA 测活请求用的浏览器 UA，避免以空 UA 被 Cloudflare 直接拦。
const checkAliveUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// chatgptAccountID 从 access_token(JWT) 解出 chatgpt_account_id，用于 chatgpt-account-id 头。
// 仅做 base64url 解码读取 payload，不校验签名。
func chatgptAccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(payload, &claims) != nil {
		return ""
	}
	return claims.Auth.AccountID
}

// probeAlive 用 access_token 请求 usage_limits 判断账号是否存活。
// 返回：alive=200 存活；transportErr=网络/超时等传输层错误（判定为"检测异常"，不改状态）；
// 其余非 200 视为失效（应停用）。code 为 HTTP 状态码，msg 为可读原因。
func probeAlive(token, accountID string) (alive bool, transportErr bool, code int, msg string) {
	req, err := http.NewRequest(http.MethodGet, usageLimitsURL, nil)
	if err != nil {
		return false, true, 0, err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", checkAliveUA)
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Referer", "https://chatgpt.com/")
	if accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}

	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return false, true, 0, err.Error()
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

	if resp.StatusCode == http.StatusOK {
		return true, false, resp.StatusCode, "存活"
	}
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	return false, false, resp.StatusCode, snippet
}

// CheckAlive 对选中账号测活：用各自 access_token 请求 usage_limits。
// 200 视为存活（若原为"停用"则恢复为已注册）；非 200 视为失效，自动置为"停用"(already_registered)；
// 传输层错误(网络/超时)判定为"检测异常"，不改状态。
// 支持单个 :id 路由或请求体 { "ids": [1,2,3] }。
func (h *Handler) CheckAlive(c *gin.Context) {
	var ids []uint
	if idParam := c.Param("id"); idParam != "" {
		n, err := strconv.ParseUint(idParam, 10, 64)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid id"})
			return
		}
		ids = []uint{uint(n)}
	} else {
		var in struct {
			IDs []uint `json:"ids"`
		}
		if err := c.ShouldBindJSON(&in); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		ids = in.IDs
	}
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "未选择账号"})
		return
	}

	var regs []models.Registration
	if err := h.DB.Where("id IN ?", ids).Find(&regs).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	var aliveN, deadN, errN int
	results := make([]gin.H, 0, len(regs))
	for i := range regs {
		reg := &regs[i]
		tok := accessToken(reg.AuthData)
		if tok == "" {
			errN++
			results = append(results, gin.H{"id": reg.ID, "email": reg.Email, "result": "error", "message": "缺少 access_token"})
			continue
		}

		ok, transportErr, statusCode, msg := probeAlive(tok, chatgptAccountID(tok))
		switch {
		case ok:
			aliveN++
			// 之前被停用、这次又存活的，恢复为已注册
			if reg.Status != "registered" {
				h.appendLog(reg, fmt.Sprintf("✔ 测活存活(HTTP %d)，恢复为已注册", statusCode))
				h.DB.Model(reg).Update("status", "registered")
			}
			results = append(results, gin.H{"id": reg.ID, "email": reg.Email, "result": "alive", "code": statusCode})
		case transportErr:
			errN++
			results = append(results, gin.H{"id": reg.ID, "email": reg.Email, "result": "error", "code": statusCode, "message": msg})
		default:
			deadN++
			h.appendLog(reg, fmt.Sprintf("✗ 测活失败(HTTP %d)，已停用: %s", statusCode, msg))
			h.DB.Model(reg).Update("status", "already_registered")
			results = append(results, gin.H{"id": reg.ID, "email": reg.Email, "result": "dead", "code": statusCode, "message": msg})
		}
	}

	c.JSON(http.StatusOK, gin.H{"alive": aliveN, "dead": deadN, "error": errN, "results": results})
}

// appendLog 给账号追加一行带时间戳的执行日志。
func (h *Handler) appendLog(reg *models.Registration, line string) {
	entry := "\n" + time.Now().Format("2006-01-02 15:04:05") + " " + line
	h.DB.Model(reg).Update("log", gorm.Expr("log || ?", entry))
}
