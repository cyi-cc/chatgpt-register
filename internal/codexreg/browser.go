package codexreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/go-rod/rod"
	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/stealth"
)

// ErrAccountTaken 注册时提示"账号不存在或已被删除/停用"，视为该地址已被注册，不应重试。
var ErrAccountTaken = errors.New("账号不存在或已被删除/停用")

// registerBrowser 启动浏览器完成 ChatGPT 账号注册并返回 accessToken。
// in.Proxy 为空则直连；非空时 Chrome 走该代理，并按出口 IP 做 GeoIP 对齐。
func registerBrowser(ctx context.Context, in Input) (token string, err error) {
	in.logf("🚀 启动浏览器自动化注册流程...")

	// 0. 按账号确定性派生一套指纹画像（同账号稳定、异账号各异，避免批量指纹雷同）
	fp := newFingerprint(in.Email)

	// 1. 启动 Chrome，禁用自动化特征
	l := launcher.New().
		NoSandbox(true).
		Set("disable-dev-shm-usage").
		Append("--disable-blink-features", "AutomationControlled").
		Append("--disable-infobars", "").
		Append("--no-first-run", "").
		Append("--no-default-browser-check", "").
		// 防 WebRTC 泄露真实 IP：只允许经代理的 UDP，STUN 不再暴露本机地址
		Set("force-webrtc-ip-handling-policy", "disable_non_proxied_udp").
		Append("--window-size", fp.windowSizeArg())
	// 无头时用 new headless（更接近有头，痕迹更少）
	if in.Headless {
		l = l.Set("headless", "new")
	} else {
		l = l.Headless(false)
	}

	// 1.1 挂代理（账号密码交给 HandleAuth）
	var proxyUser, proxyPass string
	if strings.TrimSpace(in.Proxy) != "" {
		server, user, pass, perr := parseProxy(in.Proxy)
		if perr != nil {
			return "", fmt.Errorf("解析代理失败: %w", perr)
		}
		l = l.Set("proxy-server", server)
		proxyUser, proxyPass = user, pass
		in.logf("🌐 使用代理: %s", server)
	}

	controlURL, err := l.Launch()
	if err != nil {
		return "", fmt.Errorf("启动 Chrome 失败: %w", err)
	}
	browser := rod.New().ControlURL(controlURL)
	if err := browser.Connect(); err != nil {
		return "", fmt.Errorf("连接 Chrome 失败: %w", err)
	}
	defer browser.MustClose()

	// 失败现场截图：无论是返回错误还是 MustXxx panic，都在关浏览器前把当前页面截图交给 SaveShot。
	var page *rod.Page
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("注册流程异常: %v", r)
		}
		if err == nil || page == nil || in.SaveShot == nil {
			return
		}
		func() {
			defer func() {
				if r2 := recover(); r2 != nil {
					in.logf("📸 截图失败(panic): %v", r2)
				}
			}()
			shotPage := page.CancelTimeout().Timeout(15 * time.Second)
			data, serr := shotPage.Screenshot(false, nil)
			if serr != nil {
				in.logf("📸 截图失败: %v", serr)
				return
			}
			if len(data) == 0 {
				in.logf("📸 截图失败: 空数据")
				return
			}
			in.SaveShot(data)
			in.logf("📸 已保存失败现场截图")
		}()
	}()

	// 1.2 代理需要账号密码认证时，后台处理 Chrome 弹出的认证请求。
	// 注意：必须用非 Must 版本并 recover——MustHandleAuth 在独立 goroutine 里 panic
	// 会绕过调用方的 recover 直接把整个进程带崩。
	if proxyUser != "" || proxyPass != "" {
		go func() {
			defer func() { _ = recover() }()
			wait := browser.HandleAuth(proxyUser, proxyPass)
			_ = wait()
		}()
	}

	// 2. GeoIP：先经代理出口用 HTTP 请求查询地理位置，以便创建页面时一次性注入一致指纹
	geo := lookupGeoIPViaRequest(in)
	acceptLang := "en-US,en;q=0.9"
	if geo != nil {
		_, acceptLang = localeForCountry(geo.CountryCode)
	}

	// 2.1 stealth 隐身插件 + 按真实内核对齐的 UA/Client Hints（创建即注入与地理位置一致的指纹）
	page = stealth.MustPage(browser)
	_, full := fp.applyUserAgent(page, browser, acceptLang)
	in.logf("🧬 内核=%s 指纹: %dx%d cores=%d mem=%dG gpu=%s", full, fp.screenW, fp.screenH, fp.cores, fp.memory, fp.gpu.renderer)
	// 2.2 导航前注入指纹补丁（屏幕/硬件/WebGL/Canvas/Audio），补齐 stealth 覆盖不到的项
	fp.inject(page)

	// 2.3 对齐时区/坐标/locale（UA/语言已在上面按地理信息注入）
	if geo != nil {
		applyGeo(page, geo, in)
	}

	page = page.Timeout(120 * time.Second)

	// 3. 打开 ChatGPT 注册页
	in.logf("🌐 正在打开 ChatGPT 注册页...")
	page.MustNavigate("https://chatgpt.com/auth/login")
	page.MustWaitLoad()
	page.MustElement("#email").MustWaitVisible()
	in.logf("✅ 注册页已加载")

	// 4. 输入邮箱并提交（用 JS 点击，避免元素被遮挡/未进入可点击态时 MustClick 失败）
	page.MustElement("#email").MustInput(in.Email)
	page.MustElement("button[type='submit']").MustEval(`() => this.click()`)
	in.logf("📧 已提交邮箱，等待下一步...")

	// 4.1 提交邮箱后可能出现"Create a password"创建密码页（在验证码之前）。
	// 用状态机识别：密码页则填入密码并 Continue；否则直接进入验证码环节。
	codeReady := false
	passwordDone := false
	for attempt := 0; attempt < 4 && !codeReady; attempt++ {
		pg := page.CancelTimeout().Timeout(60 * time.Second)
		state := ""
		pg.Race().
			Element("input[name='code']").MustHandle(func(_ *rod.Element) {
			state = "code"
		}).
			Element("input[type='password']").MustHandle(func(_ *rod.Element) {
			state = "password"
		}).
			MustDo()
		switch state {
		case "code":
			codeReady = true
		case "password":
			if passwordDone {
				// 密码页仍在（提交后的过渡态），稍等再重新检测，避免重复填写
				time.Sleep(2 * time.Second)
				continue
			}
			in.logf("🔒 创建密码页已出现，自动设置密码")
			pw := pg.MustElement("input[type='password']")
			pw.MustSelectAllText().MustInput(in.Password)
			pg.MustElement("button[type='submit']").MustEval(`() => this.click()`)
			passwordDone = true
			time.Sleep(2 * time.Second)
		}
	}
	if !codeReady {
		return "", fmt.Errorf("等待验证码输入框超时")
	}
	in.logf("📨 验证码输入框已出现，正在从邮箱读取验证码...")

	// 5. 自动读取验证码（由 producer 通过邮箱轮询提供）
	code, err := in.FetchCode(ctx)
	if err != nil {
		return "", fmt.Errorf("获取邮箱验证码失败: %w", err)
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return "", fmt.Errorf("未获取到验证码")
	}
	// FetchCode 轮询邮件可能耗时较久，会耗尽之前设置的页面超时预算；
	// 提交验证码前刷新一次超时，避免后续操作报 context canceled。
	page = page.CancelTimeout().Timeout(120 * time.Second)
	page.MustElement("input[name='code']").MustInput(code)
	page.MustElement("button[type='submit']").MustEval(`() => this.click()`)
	in.logf("🔑 已提交验证码")

	// 6. 提交验证码后的页面状态机：账户完善页(name/age) / 主界面 / 账号停用 /
	// "Oops, an error occurred"(Operation timed out) 报错页——点击 Try again 可继续。
	ready := false
	for attempt := 0; attempt < 8 && !ready; attempt++ {
		pg := page.CancelTimeout().Timeout(60 * time.Second)
		state := ""
		pg.Race().
			Element("textarea[name='prompt-textarea']").MustHandle(func(_ *rod.Element) {
			state = "ready"
		}).
			ElementR("body", "You do not have an account|deleted or deactivated").MustHandle(func(_ *rod.Element) {
			state = "disabled"
		}).
			ElementR("button", "Try again|重试").MustHandle(func(_ *rod.Element) {
			state = "retry"
		}).
			Element("input[name='name']").MustHandle(func(_ *rod.Element) {
			state = "profile"
		}).
			ElementR("button", "Finish creating account|完成创建账户|完成创建账号").MustHandle(func(_ *rod.Element) {
			state = "profile"
		}).
			MustDo()
		switch state {
		case "ready":
			ready = true
		case "disabled":
			return "", ErrAccountTaken
		case "retry":
			in.logf("⚠ 页面报错(Operation timed out)，点击 Try again 继续")
			pg.MustElementR("button", "Try again|重试").MustEval(`() => this.click()`)
			time.Sleep(3 * time.Second)
		case "profile":
			in.logf("📝 账户完善页面已出现")
			fillProfile(pg, in)
			time.Sleep(2 * time.Second)
		}
	}
	if !ready {
		return "", fmt.Errorf("等待 ChatGPT 主界面超时")
	}
	in.logf("✅ ChatGPT 主界面已就绪，提取 accessToken...")

	// 7. 导航到 /api/auth/session 读取 accessToken（重置超时，避免沿用已耗尽的预算）
	page = page.CancelTimeout().Timeout(60 * time.Second)
	page.MustNavigate("https://chatgpt.com/api/auth/session")
	page.MustWaitLoad()
	body := page.MustElement("body").MustText()

	var sessionData map[string]any
	if err := json.Unmarshal([]byte(body), &sessionData); err != nil {
		return "", fmt.Errorf("解析 session JSON 失败: %w", err)
	}
	accessToken, ok := sessionData["accessToken"].(string)
	if !ok || accessToken == "" {
		return "", fmt.Errorf("未找到 accessToken，可能未登录成功")
	}
	in.logf("🔑 accessToken 获取成功")
	return accessToken, nil
}

// fillProfile 填写"账户完善/确认年龄"页并提交，兼容两种版本：
//   - 旧版：Full name(input[name=name]) + Age(input[name=age])
//   - 新版：Full name + Birthday(MM/DD/YYYY 掩码或原生 date 输入) + "Finish creating account"
//
// 用标签文本/aria/placeholder + JS 定位输入框，不依赖固定 name，避免页面小改就失效。
func fillProfile(pg *rod.Page, in Input) {
	// 生成一个成年生日（沿用旧版 18~45 岁区间）；能解析出有效 Age 则据其推算年份。
	age := 18 + ri(28)
	if v, err := strconv.Atoi(strings.TrimSpace(in.Age)); err == nil && v >= 18 {
		age = v
	}
	// 在 (今天-age年) 基础上再往前随机 0~365 天，保证 ≥18 岁且生日各异。
	bday := time.Now().AddDate(-age, 0, 0).AddDate(0, 0, -ri(365))
	iso := bday.Format("2006-01-02")    // 原生 date 输入用
	mmddyyyy := bday.Format("01022006") // 掩码输入按数字逐位输入

	const js = `(fullName) => {
  const norm = s => (s||'').replace(/\s+/g,' ').trim().toLowerCase();
  const setVal = (el, v) => {
    const d = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value');
    d.set.call(el, v);
    el.dispatchEvent(new Event('input', {bubbles:true}));
    el.dispatchEvent(new Event('change', {bubbles:true}));
  };
  const byLabel = (sub) => {
    sub = norm(sub);
    for (const l of document.querySelectorAll('label')) {
      if (norm(l.textContent).includes(sub)) {
        let i = l.querySelector('input');
        if (!i && l.htmlFor) i = document.getElementById(l.htmlFor);
        if (!i) { const c = l.closest('div,fieldset,section'); if (c) i = c.querySelector('input'); }
        if (i) return i;
      }
    }
    for (const i of document.querySelectorAll('input')) {
      const a = norm(i.getAttribute('aria-label')) + ' ' + norm(i.placeholder) + ' ' + norm(i.name);
      if (a.includes(sub)) return i;
    }
    return null;
  };
  document.querySelectorAll('[data-devin-target]').forEach(e => e.removeAttribute('data-devin-target'));
  const nameInput = byLabel('full name') || document.querySelector('input[name="name"]') || byLabel('name');
  if (nameInput) setVal(nameInput, fullName);
  const ageInput = document.querySelector('input[name="age"]');
  const bInput = document.querySelector('input[name="birthday"],input[name="birthdate"],input[name="dob"],input[type="date"]') ||
                 byLabel('birthday') || byLabel('date of birth') || byLabel('birth');
  const res = {name: !!nameInput, kind: '', isDate: false};
  if (ageInput && ageInput !== bInput) {
    ageInput.setAttribute('data-devin-target', '1');
    res.kind = 'age';
  } else if (bInput) {
    bInput.setAttribute('data-devin-target', '1');
    res.kind = 'birthday';
    res.isDate = (bInput.type || '').toLowerCase() === 'date';
  }
  return JSON.stringify(res);
}`

	raw := pg.MustEval(js, in.FullName).Str()
	var info struct {
		Name   bool   `json:"name"`
		Kind   string `json:"kind"`
		IsDate bool   `json:"isDate"`
	}
	_ = json.Unmarshal([]byte(raw), &info)
	in.logf("📝 完善页字段: name=%v type=%q isDate=%v", info.Name, info.Kind, info.IsDate)

	switch info.Kind {
	case "age":
		pg.MustElement("[data-devin-target]").MustSelectAllText().MustInput(in.Age)
		in.logf("👤 已填资料 (name + age=%s)", in.Age)
	case "birthday":
		if info.IsDate {
			// 原生 date 控件用 JS 直接写 yyyy-mm-dd 更稳，逐位键入易被浏览器分段解析打乱。
			pg.MustEval(`(v) => {
  const el = document.querySelector('[data-devin-target]');
  const d = Object.getOwnPropertyDescriptor(window.HTMLInputElement.prototype, 'value');
  d.set.call(el, v);
  el.dispatchEvent(new Event('input', {bubbles:true}));
  el.dispatchEvent(new Event('change', {bubbles:true}));
}`, iso)
		} else {
			// 掩码文本框：清空默认值后逐位键入数字，由掩码自动补斜杠。
			el := pg.MustElement("[data-devin-target]")
			el.MustSelectAllText().MustInput(mmddyyyy)
		}
		in.logf("🎂 已填资料 (name + birthday=%s)", iso)
	default:
		in.logf("⚠ 完善页未识别到 age/birthday 字段，直接尝试提交")
	}
	pg.MustElement("button[type='submit']").MustEval(`() => this.click()`)
}
