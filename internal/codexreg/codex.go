package codexreg

// buildAuth 用浏览器拿到的 accessToken 组装 auth.json（accessToken 模式）。
// 注册成功后直接保存 accessToken，不解码 JWT、不向 auth.openai.com 注册 Codex agent。
func buildAuth(in Input, accessToken string) map[string]any {
	return map[string]any{
		"auth_mode":    "access_token",
		"access_token": accessToken,
		"email":        in.Email,
	}
}
