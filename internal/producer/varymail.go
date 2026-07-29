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
// 逐个购买邮箱（无母号/裂变概念），并发注册；库存用尽或余额不足即停止。
func (p *Producer) runVarymail(ctx context.Context, target int, cfg Config) {
	if strings.TrimSpace(cfg.VarymailKey) == "" || cfg.VarymailServiceID <= 0 {
		p.setMessage("未配置 varymail API Key 或服务，无法生产")
		p.logf("✗ varymail 未配置：请在设置里填写 API Key 与服务 ID")
		return
	}
	cli := varymail.New(cfg.VarymailBaseURL, cfg.VarymailKey)

	// 起始库存检查：给出友好提示，库存不足直接不跑。
	svc, err := p.varymailService(ctx, cli, cfg.VarymailServiceID)
	if err != nil {
		p.setMessage("varymail 连接失败：" + err.Error())
		p.logf("✗ varymail 查询服务失败：%v", err)
		return
	}
	p.logf("开始生产（varymail），目标 %d，服务=%s 库存=%s 可用=%d 并发 %d",
		target, svc.Name, svc.Stock, svc.Available, cfg.MaxConcurrency)
	if svc.Stock == "out" || svc.Available <= 0 {
		p.setMessage(fmt.Sprintf("varymail 服务「%s」库存不足，无法生产", svc.Name))
		p.logf("✗ varymail 库存不足（%s），本次不生产", svc.Name)
		return
	}

	sem := make(chan struct{}, cfg.MaxConcurrency)
	var wg sync.WaitGroup
	var haltMsg string // 因库存/余额等提前终止时的收尾提示

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

		// 购买一个邮箱（下单即扣费）。
		pur, bal, err := cli.Buy(ctx, cfg.VarymailServiceID)
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
		p.markInflight(email, 0)
		p.logf("🛒 varymail 分配邮箱 %s（余额 %.2f）", mask(email), bal)

		sem <- struct{}{}
		wg.Add(1)
		go func(email string, purchaseID int) {
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

			if err := p.produceOneVarymail(ctx, cfg, cli, email, purchaseID); err != nil {
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
		}(email, pur.ID)
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

// produceOneVarymail 用 varymail 分配的邮箱注册一个 ChatGPT 账号。
func (p *Producer) produceOneVarymail(ctx context.Context, cfg Config, cli *varymail.Client, email string, purchaseID int) error {
	password := codexreg.GenPassword(16)
	p.upsert(models.Registration{
		Email: email, MailboxID: 0, Password: password,
		Status: "registering", IsMother: false, Note: "varymail",
	})

	var logMu sync.Mutex
	var logBuf strings.Builder
	appendLog := func(line string) {
		logMu.Lock()
		logBuf.WriteString(time.Now().Format("2006-01-02 15:04:05") + " " + line + "\n")
		snapshot := logBuf.String()
		logMu.Unlock()
		p.db.Model(&models.Registration{}).Where("email = ?", email).Update("log", snapshot)
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
			return p.fetchCodeVarymail(ctx, cli, purchaseID)
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
		Email: email, MailboxID: 0, Password: password,
		Status: "registered", IsMother: false, Note: "varymail",
		AuthData: string(authBytes), AccountID: res.AccountID,
		UserID: res.UserID, PlanType: res.PlanType, Log: logBuf.String(),
	})
	return nil
}

// fetchCodeVarymail 轮询 varymail 取件接口，直到拿到验证码或超时。
func (p *Producer) fetchCodeVarymail(ctx context.Context, cli *varymail.Client, purchaseID int) (string, error) {
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
		case hasMail && strings.TrimSpace(msg.Code) != "":
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

// varymailService 查询指定服务的实时库存信息。
func (p *Producer) varymailService(ctx context.Context, cli *varymail.Client, serviceID int) (varymail.Service, error) {
	items, _, err := cli.Services(ctx)
	if err != nil {
		return varymail.Service{}, err
	}
	for _, s := range items {
		if s.ID == serviceID {
			return s, nil
		}
	}
	return varymail.Service{}, fmt.Errorf("服务 ID %d 不在售卖列表中", serviceID)
}
