package codexreg

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

	// 6. 提交验证码后的页面状态机：账户完善页(name/age) / 引导页 / 主界面 / 账号停用 /
	// "Oops, an error occurred"(Operation timed out) 报错页。
	// 说明：
	//   - 这里全部用非 panic 的 Race().Do()——某一轮没等到已知页面(超时)只是重试，
	//     不再像 MustDo 那样把一次超时直接抛成"注册流程异常: context deadline exceeded"。
	//   - 主界面用多个选择器判定：新版 ChatGPT 输入框是 contenteditable 的
	//     div#prompt-textarea，老选择器 textarea[name='prompt-textarea'] 已不一定命中。
	//   - 完善资料/点按钮都包在 rod.Try 里，遇到 React 重渲染导致的
	//     "Cannot find context with specified id" 时整轮重试而非直接失败。
	ready := false
	for attempt := 0; attempt < 12 && !ready; attempt++ {
		pg := page.CancelTimeout().Timeout(30 * time.Second)
		state := ""
		_, rerr := pg.Race().
			Element("#prompt-textarea").Handle(func(_ *rod.Element) error { state = "ready"; return nil }).
			Element("textarea[name='prompt-textarea']").Handle(func(_ *rod.Element) error { state = "ready"; return nil }).
			ElementR("body", "You do not have an account|deleted or deactivated").Handle(func(_ *rod.Element) error { state = "disabled"; return nil }).
			ElementR("body", "already exists|already have an account|user_already_exists").Handle(func(_ *rod.Element) error { state = "taken"; return nil }).
			ElementR("button", "Try again|重试").Handle(func(_ *rod.Element) error { state = "retry"; return nil }).
			Element("input[name='name']").Handle(func(_ *rod.Element) error { state = "profile"; return nil }).
			ElementR("button", "Okay, let's go|Continue|Next|Get started|Done|Stay logged out").Handle(func(_ *rod.Element) error { state = "next"; return nil }).
			Do()
		if rerr != nil {
			// 本轮没等到任何已知页面：稍后重试；多轮仍卡住时强制刷新主站，逼出主界面。
			if attempt >= 4 {
				_ = rod.Try(func() {
					page.CancelTimeout().Timeout(30 * time.Second).MustNavigate("https://chatgpt.com/")
				})
			}
			time.Sleep(2 * time.Second)
			continue
		}
		switch state {
		case "ready":
			ready = true
		case "disabled":
			return "", ErrAccountTaken
		case "taken":
			// user_already_exists：该邮箱已注册过，点 Try again 没用，直接判为已占用不重试。
			in.logf("⚠ 该邮箱已注册(user_already_exists)，判为已占用，不重试")
			return "", ErrAccountTaken
		case "retry":
			// "Oops, an error occurred" 页有两类：可重试的 Operation timed out，
			// 和 user_already_exists（该邮箱已注册）。后者也带 Try again 按钮，
			// 所以点之前先看正文，命中"已存在"就直接判为已占用不重试。
			taken := false
			_ = rod.Try(func() {
				t := strings.ToLower(pg.MustElement("body").MustText())
				if strings.Contains(t, "already exists") ||
					strings.Contains(t, "already have an account") ||
					strings.Contains(t, "user_already_exists") {
					taken = true
				}
			})
			if taken {
				in.logf("⚠ 该邮箱已注册(user_already_exists)，判为已占用，不重试")
				return "", ErrAccountTaken
			}
			in.logf("⚠ 页面报错(Operation timed out)，点击 Try again 继续")
			_ = rod.Try(func() { pg.MustElementR("button", "Try again|重试").MustEval(`() => this.click()`) })
			time.Sleep(3 * time.Second)
		case "next":
			in.logf("➡ 出现引导按钮，点击继续")
			_ = rod.Try(func() {
				pg.MustElementR("button", "Okay, let's go|Continue|Next|Get started|Done|Stay logged out").MustEval(`() => this.click()`)
			})
			time.Sleep(2 * time.Second)
		case "profile":
			in.logf("📝 账户完善页面已出现")
			// 资料页可能在 React 重渲染时丢失执行上下文；出错就整轮重试而非直接失败。
			ferr := rod.Try(func() {
				p2 := page.CancelTimeout().Timeout(30 * time.Second)
				name := p2.MustElement("input[name='name']")
				name.MustSelectAllText().MustInput(in.FullName)
				age := p2.MustElement("input[name='age']")
				age.MustSelectAllText().MustInput(in.Age)
				p2.MustElement("button[type='submit']").MustEval(`() => this.click()`)
			})
			if ferr != nil {
				in.logf("⚠ 完善资料时页面刷新，重试本轮")
				time.Sleep(2 * time.Second)
				continue
			}
			in.logf("👤 已提交资料 (name/age)")
			time.Sleep(3 * time.Second)
			// 提交资料后账号通常已建好：直接读 session 取 token，取到即成功返回，
			// 避免把跳转过渡态又误判成"完善页"反复重填、最终丢上下文报错。
			if tok := readAccessToken(page); tok != "" {
				in.logf("🔑 accessToken 获取成功")
				return tok, nil
			}
		}
	}

	// 7. 读取 accessToken。即使上面没等到主界面输入框，只要账号已建好，
	// /api/auth/session 通常就能拿到 token，所以直接以能否取到 token 作为最终判定。
	page = page.CancelTimeout().Timeout(60 * time.Second)
	if tok := readAccessToken(page); tok != "" {
		in.logf("🔑 accessToken 获取成功")
		return tok, nil
	}
	if !ready {
		return "", fmt.Errorf("等待 ChatGPT 主界面超时")
	}
	return "", fmt.Errorf("未找到 accessToken，可能未登录成功")
}

// readAccessToken 打开 /api/auth/session 读取 accessToken；任何异常都吞掉返回空串。
func readAccessToken(page *rod.Page) string {
	pg := page.CancelTimeout().Timeout(30 * time.Second)
	body := ""
	if err := rod.Try(func() {
		pg.MustNavigate("https://chatgpt.com/api/auth/session")
		pg.MustWaitLoad()
		body = pg.MustElement("body").MustText()
	}); err != nil {
		return ""
	}
	var sessionData map[string]any
	if json.Unmarshal([]byte(body), &sessionData) != nil {
		return ""
	}
	token, _ := sessionData["accessToken"].(string)
	return token
}
