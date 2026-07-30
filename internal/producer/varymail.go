package producer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"chatgpt-register/internal/codexreg"
	"chatgpt-register/internal/models"
	"chatgpt-register/internal/varymail"
)

// runVarymail 用 vary.email 取件作为邮箱来源生产账号：
// 与 Outlook 一样按“母号+裂变”注册（验证码都走同一取件权），
// 库存用尽或余额不足即停止。购买到的邮箱会写进邮箱管理（provider=varymail），
// 注册失败/裂变未满的留在池里，下次生产优先复用，不再重复购买。
func (p *Producer) runVarymail(ctx context.Context, target int, cfg Config) {
	if strings.TrimSpace(cfg.VarymailKey) == "" {
		p.setMessage("未配置 varymail API Key，无法生产")
		p.logf("✗ varymail 未配置：请在设置里填写 API Key")
		return
	}
	cli := varymail.New("", cfg.VarymailKey)

	// 固定使用 chatgpt 服务，起始库存检查：给出友好提示，库存不足直接不跑。
	svc, _, err := cli.ServiceByName(ctx, varymail.DefaultServiceName)
	if err != nil {
		p.setMessage("varymail 连接失败：" + err.Error())
		p.logf("✗ varymail 查询服务失败：%v", err)
		return
	}
	p.logf("开始生产（varymail），目标 %d，服务=%s 库存=%s 可用=%d 并发 %d",
		target, svc.Name, svc.Stock, svc.Available, cfg.MaxConcurrency)

	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup
	var haltMsg string // 因库存/余额等提前终止时的收尾提示

	startJob := func(mb models.Mailbox, email string, isMother bool) {
		sem <- struct{}{}
		wg.Add(1)
		go func(email string, mailboxID uint, purchaseID int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				p.releaseInflight(email)
				p.updateProgress()
			}()
			defer func() {
				if r := recover(); r != nil {
					p.markFailed(email)
					msg := fmt.Sprintf("注册异常(panic): %v", r)
					p.setRegistrationFailed(email, msg, "")
					p.logf("✗ %s %s\n%s", mask(email), msg, debug.Stack())
					p.updateProgress()
				}
			}()
			p.updateProgress()

			if err := p.produceOneVarymail(ctx, cfg, cli, email, mailboxID, purchaseID, isMother); err != nil {
				if errors.Is(err, codexreg.ErrAccountTaken) {
					p.logf("⚠ %s 停用（%v），换下一个", mask(email), err)
				} else {
					p.markFailed(email)
					p.logf("✗ %s 注册失败：%v", mask(email), err)
				}
			} else {
				p.markSuccess(email)
				p.incRegistered()
				p.logf("✓ %s 注册成功", mask(email))
			}
			p.updateProgress()
		}(email, mb.ID, mb.PurchaseID)
	}

	for {
		if ctx.Err() != nil {
			p.logf("已手动停止")
			break
		}
		done := p.producedThisRun()
		running := p.inflightCount()
		if done+running >= target {
			if running == 0 {
				break
			}
			time.Sleep(500 * time.Millisecond)
			continue
		}

		// 优先复用池里已购买的邮箱（母号未成 / 裂变未满），不重复买。
		if mb, email, isMother, ok := p.claimVarymailJob(cfg); ok {
			// 取件权可能已过期，先探测一次；失效的移出复用池。
			if _, _, cerr := cli.Code(ctx, mb.PurchaseID); errors.Is(cerr, varymail.ErrPickup) {
				p.db.Model(&models.Mailbox{}).Where("id = ?", mb.ID).
					Updates(map[string]any{"status": "verify_failed", "note": "取件权已失效"})
				p.releaseInflight(email)
				p.logf("⚠ %s 取件权已失效，移出复用池", mask(mb.Email))
				continue
			}
			if isMother {
				p.logf("♻ 复用已购买邮箱 %s", mask(email))
			} else {
				p.logf("♻ 裂变 %s（母号 %s）", mask(email), mask(mb.Email))
			}
			startJob(mb, email, isMother)
			continue
		}

		if svc.Stock == "out" || svc.Available <= 0 {
			p.setMessage(fmt.Sprintf("varymail 服务「%s」库存不足，无法继续购买", svc.Name))
			p.logf("✗ varymail 库存不足（%s），停止购买", svc.Name)
			if p.inflightCount() == 0 {
				break
			}
			time.Sleep(800 * time.Millisecond)
			continue
		}

		// 购买一个邮箱（下单即扣费）。
		pur, bal, err := cli.Buy(ctx, svc.ID)
		if err != nil {
			switch {
			case errors.Is(err, varymail.ErrOutOfStock):
				p.logf("⚠ varymail 库存已用尽，停止领取新任务")
				haltMsg = "varymail 库存不足，已停止"
			case errors.Is(err, varymail.ErrNoBalance):
				p.logf("✗ varymail 余额不足，停止生产")
				haltMsg = "varymail 余额不足，请充值"
			case errors.Is(err, varymail.ErrUnauthorized):
				p.logf("✗ varymail API Key 无效，停止生产")
				haltMsg = "varymail API Key 无效"
			default:
				p.logf("✗ varymail 下单失败：%v", err)
				haltMsg = "varymail 下单失败：" + err.Error()
			}
			// 无论哪种错误都不再开新任务，等在跑的收尾。
			if p.inflightCount() == 0 {
				break
			}
			time.Sleep(800 * time.Millisecond)
			continue
		}

		email := strings.TrimSpace(pur.Email)
		if email == "" {
			p.logf("✗ varymail 下单未返回邮箱，跳过")
			continue
		}
		// 写进邮箱管理（provider=varymail 标记），注册失败/裂变未满可复用。
		mb := p.saveVarymailMailbox(email, pur.ID)
		p.markInflight(email, mb.ID)
		p.logf("🛒 varymail 分配邮箱 %s（余额 %.2f）", mask(email), bal)

		startJob(mb, email, true)
	}

	wg.Wait()
	produced := p.producedThisRun()
	switch {
	case ctx.Err() != nil:
		p.setMessage(fmt.Sprintf("已停止，本次成功 %d 个", produced))
	case haltMsg != "":
		p.setMessage(fmt.Sprintf("%s（本次成功 %d 个）", haltMsg, produced))
	default:
		p.setMessage(fmt.Sprintf("已完成，本次成功 %d 个", produced))
	}
}

// claimVarymailJob 从邮箱管理里领取下一个 varymail 注册任务：先补齐母号，再开裂变。
// 验证码都走同一取件权，每个邮箱同一时刻只允许一个在跑任务，避免验证码串号。
func (p *Producer) claimVarymailJob(cfg Config) (models.Mailbox, string, bool, bool) {
	p.claimMu.Lock()
	defer p.claimMu.Unlock()

	var boxes []models.Mailbox
	if err := p.db.Where("provider = ? AND status = ?", SourceVarymail, "verified").
		Order("id asc").Find(&boxes).Error; err != nil {
		return models.Mailbox{}, "", false, false
	}

	// Pass 1：母号（邮箱本身地址）未注册成功且该邮箱空闲 → 注册母号
	for _, mb := range boxes {
		if mb.PurchaseID <= 0 || p.mailboxBusy(mb.ID) {
			continue
		}
		if !p.isRegistered(mb.Email) {
			p.markInflight(mb.Email, mb.ID)
			return mb, mb.Email, true, true
		}
	}

	// Pass 2：母号已成功、该邮箱空闲、裂变未满 → 注册一个新的别名子号
	for _, mb := range boxes {
		if mb.PurchaseID <= 0 || p.mailboxBusy(mb.ID) {
			continue
		}
		if !p.isRegistered(mb.Email) {
			continue
		}
		if p.fissionCount(mb) >= cfg.FissionCount {
			continue
		}
		alias := p.nextFissionEmail(mb.Email)
		if alias == "" {
			continue
		}
		p.markInflight(alias, mb.ID)
		return mb, alias, false, true
	}
	return models.Mailbox{}, "", false, false
}

// saveVarymailMailbox 把 vary.email 购买到的邮箱写进邮箱管理（provider=varymail）。
func (p *Producer) saveVarymailMailbox(email string, purchaseID int) models.Mailbox {
	var mb models.Mailbox
	if err := p.db.Where("email = ?", email).First(&mb).Error; err == nil {
		p.db.Model(&mb).Updates(map[string]any{
			"provider": SourceVarymail, "purchase_id": purchaseID, "status": "verified",
		})
		mb.Provider = SourceVarymail
		mb.PurchaseID = purchaseID
		mb.Status = "verified"
		return mb
	}
	mb = models.Mailbox{
		Email: email, Provider: SourceVarymail, PurchaseID: purchaseID,
		Status: "verified", Note: "vary.email 购买",
	}
	p.db.Create(&mb)
	return mb
}

// produceOneVarymail 用 varymail 分配的邮箱（或其别名）注册一个 ChatGPT 账号。
func (p *Producer) produceOneVarymail(ctx context.Context, cfg Config, cli *varymail.Client, email string, mailboxID uint, purchaseID int, isMother bool) error {
	password := codexreg.GenPassword(16)
	note := "varymail"
	if !isMother {
		var mb models.Mailbox
		if err := p.db.First(&mb, mailboxID).Error; err == nil {
			note = "varymail 裂变(" + mb.Email + ")"
		}
	}
	p.upsert(models.Registration{
		Email: email, MailboxID: mailboxID, Password: password,
		Status: "registering", IsMother: isMother, Note: note,
	})

	var logMu sync.Mutex
	var logBuf strings.Builder
	var existing models.Registration
	if err := p.db.Select("log").Where("email = ?", email).First(&existing).Error; err == nil && strings.TrimSpace(existing.Log) != "" {
		logBuf.WriteString(existing.Log)
		if !strings.HasSuffix(existing.Log, "\n") {
			logBuf.WriteString("\n")
		}
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05") + " --- 新一轮注册尝试 ---\n")
	}
	appendLog := func(line string) {
		logMu.Lock()
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05") + " " + line + "\n")
		snapshot := logBuf.String()
		logMu.Unlock()
		p.db.Model(&models.Registration{}).Where("email = ?", email).Update("log", snapshot)
	}

	// 同一取件权只能拿到最新一封，先记下当前最新邮件 ID，
	// 取码时跳过它，避免把上一个账号的旧验证码当成新码。
	baselineID := ""
	if msg, hasMail, err := cli.Code(ctx, purchaseID); err == nil && hasMail {
		baselineID = msg.ID
	}

	in := codexreg.Input{
		Email:    email,
		Password: password,
		Proxy:    p.nextProxy(cfg),
		Headless: cfg.Headless,
		Log: func(f string, a ...any) {
			msg := fmt.Sprintf(f, a...)
			appendLog(msg)
			p.logf("%s", "  "+mask(email)+" "+msg)
		},
		FetchCode: func(ctx context.Context) (string, error) {
			return p.fetchCodeVarymail(ctx, cli, purchaseID, baselineID)
		},
		SaveShot: func(png []byte) {
			p.db.Model(&models.Registration{}).Where("email = ?", email).Update("shot", png)
		},
	}

	res, err := codexreg.Register(ctx, in)
	if err != nil {
		if errors.Is(err, codexreg.ErrAccountTaken) {
			appendLog("⚠ 停用（账号不存在或已被删除/停用）")
			p.setRegistrationStatus(email, "already_registered", "停用："+err.Error(), logBuf.String())
			return err
		}
		appendLog("✗ 失败: " + err.Error())
		p.setRegistrationFailed(email, err.Error(), logBuf.String())
		return err
	}

	appendLog("✓ 注册成功")
	authBytes, _ := json.MarshalIndent(res.AuthJSON, "", "  ")
	p.upsert(models.Registration{
		Email: email, MailboxID: mailboxID, Password: password,
		Status: "registered", IsMother: isMother, Note: note,
		AuthData: string(authBytes), AccountID: res.AccountID,
		UserID: res.UserID, PlanType: res.PlanType, Log: logBuf.String(),
	})
	return nil
}

// fetchCodeVarymail 轮询 varymail 取件接口，直到拿到新验证码或超时；
// ignoreID 为开始前的最新邮件 ID，避免误用旧码。
func (p *Producer) fetchCodeVarymail(ctx context.Context, cli *varymail.Client, purchaseID int, ignoreID string) (string, error) {
	deadline := time.Now().Add(codePollTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		msg, hasMail, err := cli.Code(ctx, purchaseID)
		switch {
		case errors.Is(err, varymail.ErrPickup):
			// 取件暂时失败，稍后重试
		case err != nil:
			return "", err
		case hasMail && msg.ID != ignoreID && strings.TrimSpace(msg.Code) != "":
			return strings.TrimSpace(msg.Code), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(codePollInterval):
		}
	}
	return "", fmt.Errorf("超时未收到验证码")
}
