// Package varymail 封装 vary.email 取件 API：
// 查询在售服务/库存、购买取件权（分配邮箱）、按取件权拉取最新验证码。
// 作为 Outlook 本地邮箱之外的另一种"邮箱来源"，供 producer 批量注册使用。
package varymail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultBaseURL vary.email 开放 API 根地址。
const DefaultBaseURL = "https://vary.email"

// DefaultServiceName 固定使用的服务名（接收 ChatGPT/OpenAI 验证码）。
const DefaultServiceName = "chatgpt"

// 已知业务错误，供上层区分处理。
var (
	ErrUnauthorized = errors.New("varymail: API Key 缺失或无效")
	ErrNoBalance    = errors.New("varymail: 余额不足，请先充值")
	ErrOutOfStock   = errors.New("varymail: 该服务暂时无可用邮箱")
	ErrPickup       = errors.New("varymail: 取件失败，邮箱可能已失效")
	ErrNoService    = errors.New("varymail: 店铺内未找到 chatgpt 服务")
)

// Client vary.email API 客户端。
type Client struct {
	baseURL string
	apiKey  string
	http    *http.Client
}

// New 创建客户端。baseURL 为空时用默认地址。
func New(baseURL, apiKey string) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  strings.TrimSpace(apiKey),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// Service 在售服务及其库存。
type Service struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	Logo      string `json:"logo"`
	Stock     string `json:"stock"`     // ok 充足 / low 少量 / out 无库存
	Available int    `json:"available"` // 当前可用邮箱数
}

// Purchase 一次购买得到的取件权。
type Purchase struct {
	ID        int    `json:"id"`
	Email     string `json:"email"`
	Service   string `json:"service"`
	Status    string `json:"status"` // active / expired
	CreatedAt string `json:"created_at"`
}

// CodeMsg 取件权下拉取到的最新一封来信。
type CodeMsg struct {
	ID         string `json:"id"`
	From       string `json:"from"`
	Code       string `json:"code"`
	ReceivedAt string `json:"received_at"`
}

type apiEnvelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// do 发起请求并解出 data；处理统一响应包与已知错误码。
func (c *Client) do(ctx context.Context, method, path string, body any) (json.RawMessage, error) {
	if c.apiKey == "" {
		return nil, ErrUnauthorized
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if e := mapStatus(resp.StatusCode); e != nil {
		return nil, e
	}

	var env apiEnvelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("varymail: 响应解析失败(HTTP %d)", resp.StatusCode)
	}
	if env.Code != 200 {
		if e := mapStatus(env.Code); e != nil {
			return nil, e
		}
		msg := strings.TrimSpace(env.Message)
		if msg == "" {
			msg = fmt.Sprintf("code=%d", env.Code)
		}
		return nil, fmt.Errorf("varymail: %s", msg)
	}
	return env.Data, nil
}

func mapStatus(code int) error {
	switch code {
	case 401:
		return ErrUnauthorized
	case 402:
		return ErrNoBalance
	case 409:
		return ErrOutOfStock
	case 502:
		return ErrPickup
	}
	return nil
}

// Services 获取在售服务列表及取件单价。
func (c *Client) Services(ctx context.Context) (items []Service, price float64, err error) {
	data, err := c.do(ctx, http.MethodGet, "/api/shop/services", nil)
	if err != nil {
		return nil, 0, err
	}
	var out struct {
		Items []Service `json:"items"`
		Price float64   `json:"price"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, 0, fmt.Errorf("varymail: 服务列表解析失败")
	}
	return out.Items, out.Price, nil
}

// ServiceByName 在售服务里按名字找一个（大小写不敏感，先精确后包含）。
// 找不到返回 ErrNoService。price 为取件单价。
func (c *Client) ServiceByName(ctx context.Context, name string) (svc Service, price float64, err error) {
	items, price, err := c.Services(ctx)
	if err != nil {
		return Service{}, 0, err
	}
	name = strings.ToLower(strings.TrimSpace(name))
	for _, s := range items {
		if strings.ToLower(strings.TrimSpace(s.Name)) == name {
			return s, price, nil
		}
	}
	for _, s := range items {
		if strings.Contains(strings.ToLower(s.Name), name) {
			return s, price, nil
		}
	}
	return Service{}, price, ErrNoService
}

// Buy 按服务 ID 下单，成功返回分配到的邮箱与扣费后余额。
func (c *Client) Buy(ctx context.Context, serviceID int) (Purchase, float64, error) {
	data, err := c.do(ctx, http.MethodPost, "/api/my/purchases", map[string]any{"service_id": serviceID})
	if err != nil {
		return Purchase{}, 0, err
	}
	var out struct {
		Item    Purchase `json:"item"`
		Balance float64  `json:"balance"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return Purchase{}, 0, fmt.Errorf("varymail: 下单响应解析失败")
	}
	return out.Item, out.Balance, nil
}

// Code 按取件权 ID 拉取最新一封来信及提取到的验证码。
// hasMail=false 表示暂时还没有邮件（data 为空对象）。
func (c *Client) Code(ctx context.Context, purchaseID int) (msg CodeMsg, hasMail bool, err error) {
	data, err := c.do(ctx, http.MethodGet, fmt.Sprintf("/api/my/purchases/%d/code", purchaseID), nil)
	if err != nil {
		return CodeMsg{}, false, err
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "{}" || trimmed == "null" {
		return CodeMsg{}, false, nil
	}
	if err := json.Unmarshal(data, &msg); err != nil {
		return CodeMsg{}, false, fmt.Errorf("varymail: 验证码响应解析失败")
	}
	return msg, true, nil
}
