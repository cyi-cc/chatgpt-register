package models

import "time"

// Registration 一个待生产 / 已生产的 ChatGPT + Codex 账号。
//
// Status 流转:
//
//	pending(待生产) / registering(注册中) / registered(已注册) / register_failed(注册失败)
//
// 生产成功后 AuthData 存完整的 auth.json（accessToken 结构），下载时导出。
// Shipped 表示是否已"出库"（下载即出库）。
type Registration struct {
	ID        uint   `gorm:"primaryKey" json:"id"`
	Email     string `gorm:"size:255;not null;uniqueIndex" json:"email"`
	MailboxID uint   `gorm:"index" json:"mailbox_id"`
	Password  string `gorm:"size:255" json:"password"`
	Username  string `gorm:"size:255" json:"username"`

	Status  string `gorm:"size:32;default:pending" json:"status"`
	Shipped bool   `gorm:"default:false" json:"shipped"` // 出库状态：true=已出库

	// 生产结果
	AuthData  string `gorm:"type:text" json:"auth_data,omitempty"` // 完整 auth.json（含 access_token）
	AccountID string `gorm:"size:255" json:"account_id"`
	UserID    string `gorm:"size:255" json:"user_id"`
	PlanType  string `gorm:"size:32" json:"plan_type"`
	IsMother  bool   `gorm:"default:false" json:"is_mother"` // 是否母号（该邮箱主号）

	Log       string    `gorm:"type:text" json:"log,omitempty"` // 本账号执行日志
	Shot      []byte    `gorm:"type:blob" json:"-"`             // 注册失败时的页面截图(PNG)
	Note      string    `gorm:"type:text" json:"note"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
