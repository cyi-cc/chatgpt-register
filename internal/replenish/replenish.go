// Package replenish 自动补号：定时把本地已注册的 ChatGPT 账号(access_token)
// 推送到 image2api 账号池。每 30 秒查一次目标站存活的 openai 账号数，低于阈值
// 就用未出库的已注册账号补足差额（下载/推送即出库，避免重复推）。
//
// 配置来自系统设置(key-value)，可随时在设置页开关/改阈值：
//   - replenish_enabled     "1" 开启
//   - replenish_threshold   存活少于该数就补
//   - replenish_target_url  image2api 地址（如 https://host 或 http://host:port）
//   - replenish_username    image2api 后台账号（邮箱或用户名）
//   - replenish_password    image2api 后台密码
//   - replenish_autoproduce "1" 没现成号时自动生产（并发用后端「最大并发数」设置）
package replenish

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"chatgpt-register/internal/browserboot"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/producer"

	"gorm.io/gorm"
)

// maxBatch 单轮最多推送数，避免阈值配得过大时一次性爆推。
const maxBatch = 50

// pushMin 满多少个才推一次（还在生产、后面还有号进来时，攒够再推）。
const pushMin = 5

// Service 自动补号推送器：后台循环 + 缓存的 image2api 登录态。
type Service struct {
	db      *gorm.DB
	prod    *producer.Producer
	browser *browserboot.Manager
	client  *http.Client
	busy    atomic.Bool

	mu        sync.Mutex
	token     string    // 缓存的 image2api 会话 token
	tokenAt   time.Time // 本 token 的登录时间
	nextLogin time.Time // 登录失败后的冷却截止时间
}

// New 构造补号服务。prod/browser 用于「没号时自动生产」，可为 nil（仅推送已注册号）。
func New(db *gorm.DB, prod *producer.Producer, browser *browserboot.Manager) *Service {
	return &Service{
		db:      db,
		prod:    prod,
		browser: browser,
		client:  &http.Client{Timeout: 30 * time.Second},
	}
}

// Start 起后台循环，每 30 秒补一次，直到 ctx 取消。
func (s *Service) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.tick(ctx)
			}
		}
	}()
}

// tick 单轮补号。上一轮未跑完则跳过本轮，避免推送重叠。
func (s *Service) tick(ctx context.Context) {
	if !s.busy.CompareAndSwap(false, true) {
		return
	}
	defer s.busy.Store(false)

	cfg := s.settings()
	if cfg["replenish_enabled"] != "1" {
		return
	}
	threshold, _ := strconv.Atoi(strings.TrimSpace(cfg["replenish_threshold"]))
	base := strings.TrimRight(strings.TrimSpace(cfg["replenish_target_url"]), "/")
	user := strings.TrimSpace(cfg["replenish_username"])
	pass := cfg["replenish_password"]
	if threshold <= 0 || base == "" || user == "" || pass == "" {
		return
	}

	alive, err := s.aliveCount(ctx, base, user, pass)
	if err != nil {
		log.Printf("补号：查询目标存活数失败：%v", err)
		return
	}
	need := threshold - alive
	if need <= 0 {
		return
	}

	// 现成可推的号：已注册、未出库、带 access_token。
	var ready int64
	s.db.Model(&models.Registration{}).
		Where("status = ? AND shipped = ? AND auth_data <> ''", "registered", false).
		Count(&ready)

	// 没号时自动生产：现成号补不满差额就滚动生产，并发用后端「最大并发数」设置。
	moreComing := s.prod != nil && s.prod.Snapshot().Running
	if cfg["replenish_autoproduce"] == "1" && s.prod != nil {
		if want := need - int(ready); want > 0 && !moreComing {
			if s.browser != nil && s.browser.Ready() {
				if err := s.prod.Start(want); err != nil {
					log.Printf("补号：自动生产未启动：%v", err)
				} else {
					moreComing = true
					log.Printf("补号：目标存活 %d < 阈值 %d、现成号 %d 不足，自动生产 %d 个", alive, threshold, ready, want)
				}
			} else {
				log.Printf("补号：需自动生产但浏览器未就绪，跳过本轮")
			}
		}
	}

	// 满 pushMin 个才推一次：不足且后面还有号在生产就等攒够，否则把剩下的也推掉。
	pushN := need
	if int(ready) < pushN {
		pushN = int(ready)
	}
	if pushN < pushMin && moreComing {
		return
	}
	if pushN <= 0 {
		return
	}
	if pushN > maxBatch {
		pushN = maxBatch
	}

	var regs []models.Registration
	if err := s.db.WithContext(ctx).
		Where("status = ? AND shipped = ? AND auth_data <> ''", "registered", false).
		Order("id asc").Limit(pushN).Find(&regs).Error; err != nil {
		log.Printf("补号：查询待推账号失败：%v", err)
		return
	}

	pushed := 0
	for _, r := range regs {
		tok := accessToken(r.AuthData)
		if tok == "" {
			continue
		}
		if err := s.importToken(ctx, base, user, pass, tok); err != nil {
			log.Printf("补号：推送账号 #%d 失败：%v", r.ID, err)
			continue
		}
		if err := s.db.Model(&models.Registration{}).Where("id = ?", r.ID).Update("shipped", true).Error; err != nil {
			log.Printf("补号：标记 #%d 出库失败：%v", r.ID, err)
		}
		pushed++
	}
	if pushed > 0 {
		log.Printf("补号：目标存活 %d < 阈值 %d，已推送 %d 个账号", alive, threshold, pushed)
	}
}

// settings 读取全部系统设置为 map。
func (s *Service) settings() map[string]string {
	var items []models.Setting
	if err := s.db.Find(&items).Error; err != nil {
		log.Printf("补号：读取设置失败：%v", err)
		return map[string]string{}
	}
	m := make(map[string]string, len(items))
	for _, it := range items {
		m[it.Key] = it.Value
	}
	return m
}

// aliveCount 查 image2api 存活的 openai 账号数。account 统计里 openai 含
// n(总)/ok(正常)/dead(失效)/quota(限额)；存活 = 正常 + 待验证(刚推入的) = n-dead-quota，
// 这样刚推进去还在 pending 的号也计入，避免 30 秒内重复补。
func (s *Service) aliveCount(ctx context.Context, base, user, pass string) (int, error) {
	resp, err := s.authedRequest(ctx, base, user, pass, http.MethodGet, "/admin/api/accounts", nil)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Stats struct {
			Openai struct {
				N     int `json:"n"`
				Ok    int `json:"ok"`
				Dead  int `json:"dead"`
				Quota int `json:"quota"`
			} `json:"openai"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return 0, fmt.Errorf("解析响应失败：%w", err)
	}
	st := out.Stats.Openai
	alive := st.N - st.Dead - st.Quota
	if alive < 0 {
		alive = 0
	}
	return alive, nil
}

// importToken 把一个 access_token 推入 image2api 的 chatgpt 池。
func (s *Service) importToken(ctx context.Context, base, user, pass, token string) error {
	body, _ := json.Marshal(map[string]string{"access_token": token})
	resp, err := s.authedRequest(ctx, base, user, pass, http.MethodPost, "/admin/api/tokens/import-chatgpt-token", body)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	return nil
}

// authedRequest 带登录态发请求；遇 401 清缓存重登一次再试。
func (s *Service) authedRequest(ctx context.Context, base, user, pass, method, path string, body []byte) (*http.Response, error) {
	send := func(tok string) (*http.Response, error) {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, base+path, rdr)
		if err != nil {
			return nil, err
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Authorization", "Bearer "+tok)
		return s.client.Do(req)
	}

	tok, err := s.ensureToken(ctx, base, user, pass)
	if err != nil {
		return nil, err
	}
	resp, err := send(tok)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		resp.Body.Close()
		s.mu.Lock()
		s.token = ""
		s.mu.Unlock()
		if tok, err = s.ensureToken(ctx, base, user, pass); err != nil {
			return nil, err
		}
		return send(tok)
	}
	return resp, nil
}

// ensureToken 返回可用的会话 token：缓存 1 小时；登录失败则冷却 5 分钟再试，
// 避免账密配错时每 30 秒狂敲登录接口触发对方限流。
func (s *Service) ensureToken(ctx context.Context, base, user, pass string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token != "" && time.Since(s.tokenAt) < time.Hour {
		return s.token, nil
	}
	if !s.nextLogin.IsZero() && time.Now().Before(s.nextLogin) {
		return "", fmt.Errorf("登录冷却中，稍后重试")
	}
	tok, err := s.login(ctx, base, user, pass)
	if err != nil {
		s.nextLogin = time.Now().Add(5 * time.Minute)
		return "", err
	}
	s.nextLogin = time.Time{}
	s.token = tok
	s.tokenAt = time.Now()
	return tok, nil
}

// login 用配置里的账密登录 image2api，取会话 token。
func (s *Service) login(ctx context.Context, base, user, pass string) (string, error) {
	body, _ := json.Marshal(map[string]string{"identifier": user, "password": pass})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/admin/api/auth/login", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("登录失败 HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(data, &out); err != nil || out.Token == "" {
		return "", fmt.Errorf("登录响应缺少 token")
	}
	return out.Token, nil
}

// accessToken 从库里存的 auth.json 提取 access_token。
func accessToken(authData string) string {
	var parsed map[string]any
	_ = json.Unmarshal([]byte(authData), &parsed)
	s, _ := parsed["access_token"].(string)
	return s
}
