package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"chatgpt-register/internal/emailalias"
	"chatgpt-register/internal/mailfetch"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/varymail"

	"github.com/gin-gonic/gin"
)

type mailboxInput struct {
	Email        string `json:"email" binding:"required"`
	Password     string `json:"password"`
	Provider     string `json:"provider"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
	Status       string `json:"status"`
	Note         string `json:"note"`
}

var mailboxStatuses = map[string]bool{
	"unverified":    true,
	"verifying":     true,
	"verify_failed": true,
	"verified":      true,
}

func validMailboxStatus(s string) bool {
	return s == "" || mailboxStatuses[s]
}

func (h *Handler) MailboxList(c *gin.Context) {
	var items []models.Mailbox
	q := h.DB.Order("id desc")
	if s := c.Query("status"); s != "" {
		q = q.Where("status = ?", s)
	}
	if kw := c.Query("q"); kw != "" {
		like := "%" + kw + "%"
		q = q.Where("email LIKE ? OR provider LIKE ? OR note LIKE ?", like, like, like)
	}
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	var total int64
	q.Model(&models.Mailbox{}).Count(&total)
	if err := q.Offset((page - 1) * size).Limit(size).Find(&items).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	registerLimit := 1 + h.fissionCount()
	for i := range items {
		items[i].RegisterCount = h.mailboxRegisterCount(items[i])
		items[i].RegisterLimit = registerLimit
	}
	c.JSON(http.StatusOK, gin.H{"data": items, "total": total, "page": page, "size": size})
}

func (h *Handler) fissionCount() int {
	var s models.Setting
	if err := h.DB.Where("key = ?", "fission_count").First(&s).Error; err != nil {
		return 5
	}
	n, err := strconv.Atoi(strings.TrimSpace(s.Value))
	if err != nil || n < 0 {
		return 5
	}
	return n
}

func (h *Handler) mailboxRegisterCount(m models.Mailbox) int {
	var n int64
	// 归属该邮箱的记录：本身地址、mailbox_id 或别名（母号 + 裂变子号）
	match := h.DB.Where("mailbox_id = ? OR email = ?", m.ID, m.Email)
	if pattern := emailalias.LikePattern(m.Email); pattern != "" {
		match = match.Or("email LIKE ? ESCAPE '\\'", pattern)
	}
	// 只统计已占用名额的记录（成功注册 / 停用），pending / 失败可重试不计入
	h.DB.Model(&models.Registration{}).
		Where(match).
		Where("status IN ?", []string{"registered", "already_registered"}).
		Count(&n)
	return int(n)
}

func (h *Handler) MailboxCreate(c *gin.Context) {
	var in mailboxInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validMailboxStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	m := models.Mailbox{Email: in.Email, Password: in.Password, Provider: in.Provider, ClientID: in.ClientID, RefreshToken: in.RefreshToken, Status: in.Status, Note: in.Note}
	if m.Status == "" {
		m.Status = "unverified"
	}
	if err := h.DB.Create(&m).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusCreated, m)
}

type mailboxImportItem struct {
	Email        string `json:"email"`
	Password     string `json:"password"`
	ClientID     string `json:"client_id"`
	RefreshToken string `json:"refresh_token"`
}

// MailboxImport 批量导入邮箱，重复 email 自动跳过。
func (h *Handler) MailboxImport(c *gin.Context) {
	var in struct {
		Items []mailboxImportItem `json:"items" binding:"required"`
	}
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	added, skipped := 0, 0
	seen := map[string]bool{}
	for _, it := range in.Items {
		email := strings.TrimSpace(it.Email)
		if email == "" || !strings.Contains(email, "@") || seen[email] {
			skipped++
			continue
		}
		seen[email] = true
		var count int64
		h.DB.Model(&models.Mailbox{}).Where("email = ?", email).Count(&count)
		if count > 0 {
			skipped++
			continue
		}
		m := models.Mailbox{
			Email:        email,
			Password:     strings.TrimSpace(it.Password),
			ClientID:     strings.TrimSpace(it.ClientID),
			RefreshToken: strings.TrimSpace(it.RefreshToken),
			Status:       "verifying",
		}
		if err := h.DB.Create(&m).Error; err != nil {
			skipped++
			continue
		}
		added++
	}
	c.JSON(http.StatusOK, gin.H{"added": added, "skipped": skipped})
}

// MailboxVerify 校验单个邮箱凭据是否可用，更新状态为 verified / verify_failed。
func (h *Handler) MailboxVerify(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	if m.Provider == "varymail" {
		// varymail 邮箱验证取件权是否仍有效
		var err error
		if key := h.setting("varymail_api_key"); key == "" || m.PurchaseID <= 0 {
			err = varymail.ErrUnauthorized
		} else {
			_, _, err = varymail.New("", key).Code(c.Request.Context(), m.PurchaseID)
		}
		if err != nil {
			m.Status = "verify_failed"
		} else {
			m.Status = "verified"
		}
		h.DB.Model(&m).Update("status", m.Status)
		c.JSON(http.StatusOK, gin.H{"id": m.ID, "status": m.Status})
		return
	}
	err := h.Mail.Verify(c.Request.Context(), mailfetch.Account{
		Email:        m.Email,
		ClientID:     m.ClientID,
		RefreshToken: m.RefreshToken,
	})
	if err != nil {
		m.Status = "verify_failed"
	} else {
		m.Status = "verified"
	}
	h.DB.Model(&m).Update("status", m.Status)
	c.JSON(http.StatusOK, gin.H{"id": m.ID, "status": m.Status})
}

func (h *Handler) MailboxUpdate(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
		return
	}
	var in mailboxInput
	if err := c.ShouldBindJSON(&in); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !validMailboxStatus(in.Status) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
		return
	}
	m.Email = in.Email
	m.Password = in.Password
	m.Provider = in.Provider
	m.ClientID = in.ClientID
	m.RefreshToken = in.RefreshToken
	if in.Status != "" {
		m.Status = in.Status
	}
	m.Note = in.Note
	if err := h.DB.Save(&m).Error; err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, m)
}

func (h *Handler) MailboxDelete(c *gin.Context) {
	if err := h.DB.Delete(&models.Mailbox{}, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// MailboxMessages 取件：拉某个邮箱收件箱最新邮件，含完整 HTML 正文，供网页弹窗轮询展示。
func (h *Handler) MailboxMessages(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	if m.Provider == "varymail" {
		h.varymailMessages(c, m)
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "20"))
	msgs, err := h.Mail.ListMessages(c.Request.Context(), mailfetch.Account{
		Email:        m.Email,
		ClientID:     m.ClientID,
		RefreshToken: m.RefreshToken,
	}, limit)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": m.Email})
		return
	}
	c.JSON(http.StatusOK, gin.H{"email": m.Email, "items": msgs})
}

// MailboxMessage 按消息 ID 拉取单封邮件的完整正文，供点击后按需加载。
func (h *Handler) MailboxMessage(c *gin.Context) {
	var m models.Mailbox
	if err := h.DB.First(&m, c.Param("id")).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "邮箱不存在"})
		return
	}
	if m.Provider == "varymail" {
		h.varymailMessage(c, m)
		return
	}
	msg, err := h.Mail.GetMessage(c.Request.Context(), mailfetch.Account{
		Email:        m.Email,
		ClientID:     m.ClientID,
		RefreshToken: m.RefreshToken,
	}, c.Query("mid"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, mailfetch.ErrMissingCreds) || errors.Is(err, mailfetch.ErrAuthFailed) {
			status = http.StatusBadRequest
		}
		c.JSON(status, gin.H{"error": err.Error(), "email": m.Email})
		return
	}
	c.JSON(http.StatusOK, msg)
}

// varymailMessages 用取件权拉取 varymail 邮箱最新一封来信（vary.email 只提供最新一封）。
func (h *Handler) varymailMessages(c *gin.Context, m models.Mailbox) {
	key := h.setting("varymail_api_key")
	if key == "" || m.PurchaseID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 varymail API Key 或取件权 ID", "email": m.Email})
		return
	}
	msg, hasMail, err := varymail.New("", key).Code(c.Request.Context(), m.PurchaseID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error(), "email": m.Email})
		return
	}
	items := []gin.H{}
	if hasMail {
		items = append(items, gin.H{
			"id": msg.ID, "from": msg.From, "from_name": "",
			"subject": "验证码：" + msg.Code, "received_at": msg.ReceivedAt,
		})
	}
	c.JSON(http.StatusOK, gin.H{"email": m.Email, "items": items})
}

// varymailMessage varymail 只有验证码，正文直接返回验证码文本。
func (h *Handler) varymailMessage(c *gin.Context, m models.Mailbox) {
	key := h.setting("varymail_api_key")
	if key == "" || m.PurchaseID <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "缺少 varymail API Key 或取件权 ID", "email": m.Email})
		return
	}
	msg, hasMail, err := varymail.New("", key).Code(c.Request.Context(), m.PurchaseID)
	if err != nil || !hasMail {
		c.JSON(http.StatusBadRequest, gin.H{"error": "暂无邮件", "email": m.Email})
		return
	}
	c.JSON(http.StatusOK, gin.H{"id": msg.ID, "from": msg.From, "subject": "验证码：" + msg.Code, "text": "验证码：" + msg.Code, "received_at": msg.ReceivedAt})
}
