package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	bp "zai-proxy/internal/browserproxy"
)

type AnonymousAuthResponse struct {
	Token string `json:"token"`
}

// GetAnonymousToken 从 z.ai 获取匿名 token
func GetAnonymousToken() (string, error) {
	req, err := http.NewRequest("GET", "https://chat.z.ai/api/v1/auths/", nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("status %d: %s", resp.StatusCode, string(body[:min(200, len(body))]))
	}

	var authResp AnonymousAuthResponse
	if err := json.NewDecoder(resp.Body).Decode(&authResp); err != nil {
		return "", fmt.Errorf("decode failed: %w", err)
	}

	if strings.TrimSpace(authResp.Token) == "" {
		return "", fmt.Errorf("empty token received")
	}

	return authResp.Token, nil
}

// Cached captcha verification param (not used in auto mode)
var (
	cachedCaptchaParam string
	captchaParamLock   sync.RWMutex
	captchaParamExpiry time.Time
)

// GetCaptchaVerifyParam 获取验证码验证参数
func GetCaptchaVerifyParam() (string, error) {
	captchaParamLock.RLock()
	if cachedCaptchaParam != "" && time.Now().Before(captchaParamExpiry) {
		param := cachedCaptchaParam
		captchaParamLock.RUnlock()
		return param, nil
	}
	captchaParamLock.RUnlock()

	if Cfg != nil && Cfg.CaptchaVerifyParam != "" {
		return Cfg.CaptchaVerifyParam, nil
	}

	return "", fmt.Errorf("captcha_verify_param not available")
}

// SetCaptchaVerifyParam 设置 captcha_verify_param
func SetCaptchaVerifyParam(param string) {
	captchaParamLock.Lock()
	defer captchaParamLock.Unlock()
	cachedCaptchaParam = param
	captchaParamExpiry = time.Now().Add(30 * time.Minute)
	LogInfo("[Captcha] Captcha verify param updated, expires in 30 minutes")
}

// CaptchaSession holds a token + captcha pair.
type CaptchaSession struct {
	Token              string
	CaptchaVerifyParam string
	Cookies            string
	SolvedAt           time.Time
}

// GetCaptchaSession returns a session from the browser proxy pool.
func GetCaptchaSession() *CaptchaSession {
	session := bp.GetSession()
	if session == nil {
		return nil
	}
	return &CaptchaSession{
		Token:   session.Token,
		Cookies: session.Cookies,
		SolvedAt: session.Created,
	}
}
