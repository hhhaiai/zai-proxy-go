package internal

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type AnonymousAuthResponse struct {
	Token string `json:"token"`
}

// Cached captcha verification param
var (
	cachedCaptchaParam string
	captchaParamLock   sync.RWMutex
	captchaParamExpiry time.Time
)

// GetAnonymousToken 从 z.ai 获取匿名 token
func GetAnonymousToken() (string, error) {
	req, err := http.NewRequest("GET", "https://chat.z.ai/api/v1/auths/", nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	req.Header.Set("Accept-Language", "zh-CN")
	req.Header.Set("Cache-Control", "no-cache")
	req.Header.Set("Connection", "keep-alive")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Host", "chat.z.ai")
	req.Header.Set("Pragma", "no-cache")
	req.Header.Set("Sec-Fetch-Dest", "empty")
	req.Header.Set("Sec-Fetch-Mode", "cors")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36")

	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSHandshakeTimeout: 5 * time.Second,
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		bodyStr := string(body)
		if len(bodyStr) > 200 {
			bodyStr = bodyStr[:200]
		}
		return "", fmt.Errorf("status %d, body: %s", resp.StatusCode, bodyStr)
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

// GetCaptchaVerifyParam 获取验证码验证参数
func GetCaptchaVerifyParam() (string, error) {
	// Check cache first
	captchaParamLock.RLock()
	if cachedCaptchaParam != "" && time.Now().Before(captchaParamExpiry) {
		param := cachedCaptchaParam
		captchaParamLock.RUnlock()
		return param, nil
	}
	captchaParamLock.RUnlock()

	// If configured via env, use that
	if Cfg != nil && Cfg.CaptchaVerifyParam != "" {
		return Cfg.CaptchaVerifyParam, nil
	}

	// Return error - captcha must be provided via /captcha page or env
	return "", fmt.Errorf("captcha_verify_param not available. Visit http://localhost:%s/captcha to solve captcha", getPort())
}

// SetCaptchaVerifyParam 设置 captcha_verify_param (由 /captcha 页面调用)
func SetCaptchaVerifyParam(param string) {
	captchaParamLock.Lock()
	defer captchaParamLock.Unlock()
	cachedCaptchaParam = param
	captchaParamExpiry = time.Now().Add(30 * time.Minute)
	LogInfo("[Captcha] Captcha verify param updated, expires in 30 minutes")
}

// BuildCaptchaVerifyParam 构建 captcha_verify_param (base64 encoded JSON)
func BuildCaptchaVerifyParam(certifyID, sceneID, securityToken string) string {
	param := map[string]interface{}{
		"certifyId":     certifyID,
		"sceneId":       sceneID,
		"isSign":        true,
		"securityToken": securityToken,
	}
	data, _ := json.Marshal(param)
	return base64.StdEncoding.EncodeToString(data)
}

func getPort() string {
	if Cfg != nil && Cfg.Port != "" {
		return Cfg.Port
	}
	return "8000"
}

// CaptchaSolverResult Puppeteer脚本输出的结果
type CaptchaSolverResult struct {
	Token               string `json:"token"`
	SecurityToken       string `json:"securityToken"`
	CertifyID           string `json:"certifyId"`
	CaptchaVerifyParam  string `json:"captchaVerifyParam"`
	HasToken            bool   `json:"hasToken"`
	HasSecurityToken    bool   `json:"hasSecurityToken"`
	Error               string `json:"error,omitempty"`
}

// RunCaptchaSolver 运行 Puppeteer 脚本解决验证码
func RunCaptchaSolver() (*CaptchaSolverResult, error) {
	// Find the captcha_solver.js script
	execPath, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("failed to get executable path: %w", err)
	}
	scriptDir := filepath.Dir(execPath)
	scriptPath := filepath.Join(scriptDir, "captcha_solver.js")

	// Also check current working directory
	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		cwd, _ := os.Getwd()
		scriptPath = filepath.Join(cwd, "captcha_solver.js")
	}

	if _, err := os.Stat(scriptPath); os.IsNotExist(err) {
		return nil, fmt.Errorf("captcha_solver.js not found")
	}

	LogInfo("[Captcha] Running Puppeteer captcha solver...")

	// Run the script
	ctx, cancel := timeoutContext(90 * time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", scriptPath)
	cmd.Dir = filepath.Dir(scriptPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("failed to create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("failed to start captcha solver: %w", err)
	}

	// Read stderr in background (for logging)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				LogDebug("[Captcha-Solver] %s", string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	// Read stdout (JSON result)
	stdoutBytes, err := io.ReadAll(stdout)
	if err != nil {
		return nil, fmt.Errorf("failed to read stdout: %w", err)
	}

	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("captcha solver failed: %w, output: %s", err, string(stdoutBytes))
	}

	var result CaptchaSolverResult
	if err := json.Unmarshal(stdoutBytes, &result); err != nil {
		return nil, fmt.Errorf("failed to parse solver output: %w, output: %s", err, string(stdoutBytes))
	}

	if result.Error != "" {
		return nil, fmt.Errorf("solver error: %s", result.Error)
	}

	if !result.HasSecurityToken {
		return nil, fmt.Errorf("solver did not capture securityToken")
	}

	LogInfo("[Captcha] Puppeteer solver succeeded! certifyId=%s", result.CertifyID)
	return &result, nil
}

// timeoutContext 创建带超时的 context
func timeoutContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), timeout)
}

// AutoSolveCaptcha 自动解决验证码并缓存
func AutoSolveCaptcha() {
	result, err := RunCaptchaSolver()
	if err != nil {
		LogWarn("[Captcha] Auto-solve failed: %v", err)
		return
	}

	if result.CaptchaVerifyParam != "" {
		SetCaptchaVerifyParam(result.CaptchaVerifyParam)
		LogInfo("[Captcha] Auto-solve complete, captcha_verify_param cached")
	}
}

// StartCaptchaRefresh 定期刷新验证码
func StartCaptchaRefresh() {
	go func() {
		for {
			time.Sleep(25 * time.Minute) // Refresh before 30-min expiry
			LogInfo("[Captcha] Refreshing captcha token...")
			AutoSolveCaptcha()
		}
	}()
}

// CaptchaPageHTML 验证码页面 HTML
const CaptchaPageHTML = `<!DOCTYPE html>
<html>
<head>
    <meta charset="UTF-8">
    <title>Z.AI Proxy - Captcha Verification</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', sans-serif; max-width: 600px; margin: 50px auto; padding: 20px; background: #f5f5f5; }
        .card { background: white; border-radius: 12px; padding: 30px; box-shadow: 0 2px 10px rgba(0,0,0,0.1); }
        h1 { color: #333; font-size: 24px; margin-bottom: 10px; }
        p { color: #666; line-height: 1.6; }
        .status { padding: 12px; border-radius: 8px; margin: 15px 0; }
        .status.ok { background: #e8f5e9; color: #2e7d32; }
        .status.error { background: #ffebee; color: #c62828; }
        .status.pending { background: #fff3e0; color: #e65100; }
        #captcha-container { margin: 20px 0; min-height: 60px; }
        .btn { background: #1976d2; color: white; border: none; padding: 12px 24px; border-radius: 8px; cursor: pointer; font-size: 16px; }
        .btn:hover { background: #1565c0; }
        .btn:disabled { background: #ccc; cursor: not-allowed; }
        .info { font-size: 14px; color: #888; margin-top: 20px; }
    </style>
</head>
<body>
    <div class="card">
        <h1>Z.AI Proxy - Captcha Verification</h1>
        <p>This page solves the captcha required for anonymous mode. Click the button below to verify.</p>

        <div id="status" class="status pending">
            Waiting for captcha verification...
        </div>

        <div id="captcha-container"></div>

        <button id="verifyBtn" class="btn" onclick="startVerification()">
            Start Verification
        </button>

        <p class="info">
            The captcha token will be cached for 30 minutes.<br>
            You only need to do this once per session.
        </p>
    </div>

    <script>
        // Intercept both fetch and XHR to capture VerifyCaptchaV3 response
        var capturedSecurityToken = '';
        var capturedCertifyId = '';

        // Intercept fetch
        var origFetch = window.fetch;
        window.fetch = function() {
            var url = arguments[0];
            if (typeof url === 'string' && url.indexOf('VerifyCaptchaV3') !== -1) {
                return origFetch.apply(this, arguments).then(function(response) {
                    var cloned = response.clone();
                    cloned.text().then(function(text) {
                        try {
                            var data = JSON.parse(text);
                            if (data && data.Result && data.Result.securityToken) {
                                capturedSecurityToken = data.Result.securityToken;
                                capturedCertifyId = data.Result.certifyId || '';
                                console.log('[Fetch] Captured securityToken');
                            }
                        } catch(e) {}
                    }).catch(function() {});
                    return response;
                });
            }
            return origFetch.apply(this, arguments);
        };

        // Intercept XMLHttpRequest
        var origXHROpen = XMLHttpRequest.prototype.open;
        var origXHRSend = XMLHttpRequest.prototype.send;
        XMLHttpRequest.prototype.open = function(method, url) {
            this._interceptUrl = url;
            return origXHROpen.apply(this, arguments);
        };
        XMLHttpRequest.prototype.send = function() {
            var xhr = this;
            if (xhr._interceptUrl && xhr._interceptUrl.indexOf('VerifyCaptchaV3') !== -1) {
                var origOnReady = xhr.onreadystatechange;
                xhr.onreadystatechange = function() {
                    if (xhr.readyState === 4 && xhr.responseText) {
                        try {
                            var data = JSON.parse(xhr.responseText);
                            if (data && data.Result && data.Result.securityToken) {
                                capturedSecurityToken = data.Result.securityToken;
                                capturedCertifyId = data.Result.certifyId || '';
                                console.log('[XHR] Captured securityToken');
                            }
                        } catch(e) {}
                    }
                    if (origOnReady) origOnReady.apply(this, arguments);
                };
            }
            return origXHRSend.apply(this, arguments);
        };

        function startVerification() {
            var btn = document.getElementById('verifyBtn');
            btn.disabled = true;
            btn.textContent = 'Verifying...';
            document.getElementById('status').className = 'status pending';
            document.getElementById('status').textContent = 'Initializing captcha...';

            var script = document.createElement('script');
            script.src = 'https://o.alicdn.com/captcha-frontend/aliyunCaptcha/AliyunCaptcha.js';
            script.onload = function() { initCaptcha(); };
            script.onerror = function() {
                document.getElementById('status').className = 'status error';
                document.getElementById('status').textContent = 'Failed to load captcha SDK';
                btn.disabled = false;
                btn.textContent = 'Retry';
            };
            document.head.appendChild(script);
        }

        function initCaptcha() {
            if (!window.AliyunCaptcha) {
                document.getElementById('status').className = 'status error';
                document.getElementById('status').textContent = 'Captcha SDK not loaded';
                return;
            }

            window.AliyunCaptcha.init({
                SceneId: 'didk33e0',
                mode: 'popup',
                region: 'cn',
                element: '#captcha-container',
                captchaVerifyCallback: function(param, next) {
                    console.log('captchaVerifyCallback param:', JSON.stringify(param).substring(0, 200));

                    // Wait a moment for the intercepted fetch to capture securityToken
                    return new Promise(function(resolve) {
                        setTimeout(function() {
                            var paramObj = param;
                            if (typeof param === 'string') {
                                try { paramObj = JSON.parse(param); } catch(e) {}
                            }

                            // Build captcha_verify_param from captured data
                            var captchaVerifyParam = '';
                            if (capturedSecurityToken) {
                                var verifyData = {
                                    certifyId: capturedCertifyId || paramObj.certifyId || '',
                                    sceneId: 'didk33e0',
                                    isSign: true,
                                    securityToken: capturedSecurityToken
                                };
                                captchaVerifyParam = btoa(JSON.stringify(verifyData));
                            }

                            // Send to our proxy server
                            fetch('/captcha/verify', {
                                method: 'POST',
                                headers: {'Content-Type': 'application/json'},
                                body: JSON.stringify({
                                    captchaVerifyParam: captchaVerifyParam,
                                    rawParam: paramObj,
                                    securityToken: capturedSecurityToken,
                                    certifyId: capturedCertifyId
                                })
                            }).then(function(r) { return r.json(); })
                            .then(function(data) {
                                if (data.success) {
                                    document.getElementById('status').className = 'status ok';
                                    document.getElementById('status').textContent = 'Captcha verified! Token cached for 30 minutes.';
                                    document.getElementById('verifyBtn').textContent = 'Done!';
                                    resolve({captchaResult: 'PASS'});
                                } else {
                                    document.getElementById('status').className = 'status error';
                                    document.getElementById('status').textContent = 'Failed: ' + (data.error || 'unknown');
                                    document.getElementById('verifyBtn').disabled = false;
                                    document.getElementById('verifyBtn').textContent = 'Retry';
                                    resolve({captchaResult: 'FAIL'});
                                }
                            }).catch(function(err) {
                                document.getElementById('status').className = 'status error';
                                document.getElementById('status').textContent = 'Error: ' + err.message;
                                document.getElementById('verifyBtn').disabled = false;
                                document.getElementById('verifyBtn').textContent = 'Retry';
                                resolve({captchaResult: 'FAIL'});
                            });
                        }, 500);
                    });
                },
                slideBehaviorCallback: function(param) {
                    console.log('slideBehaviorCallback:', param);
                }
            });

            setTimeout(function() {
                if (window.AliyunCaptcha && window.AliyunCaptcha.showBind) {
                    window.AliyunCaptcha.showBind();
                }
            }, 500);
        }
    </script>
</body>
</html>`

// HandleCaptchaPage 处理验证码页面请求
func HandleCaptchaPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	fmt.Fprint(w, CaptchaPageHTML)
}

// HandleCaptchaVerify 处理验证码验证回调
func HandleCaptchaVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	bodyBytes, _ := io.ReadAll(r.Body)
	LogDebug("[Captcha] Verify callback body: %s", string(bodyBytes[:min(500, len(bodyBytes))]))

	var rawParam map[string]interface{}
	if err := json.Unmarshal(bodyBytes, &rawParam); err != nil {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Invalid JSON",
		})
		return
	}

	var captchaParam string

	// Priority 1: Use pre-built captchaVerifyParam (from intercepted fetch)
	if verifyParam, ok := rawParam["captchaVerifyParam"].(string); ok && verifyParam != "" {
		captchaParam = verifyParam
		LogInfo("[Captcha] Using pre-built captchaVerifyParam")
	}

	// Priority 2: Build from securityToken + certifyId
	if captchaParam == "" {
		securityToken, _ := rawParam["securityToken"].(string)
		certifyId, _ := rawParam["certifyId"].(string)
		if securityToken != "" && certifyId != "" {
			captchaParam = BuildCaptchaVerifyParam(certifyId, "didk33e0", securityToken)
			LogInfo("[Captcha] Built param from intercepted securityToken")
		}
	}

	if captchaParam == "" {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"success": false,
			"error":   "Could not build captcha param. securityToken not captured.",
		})
		return
	}

	SetCaptchaVerifyParam(captchaParam)
	LogInfo("[Captcha] Captcha param cached successfully")

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"success": true,
		"message": "Captcha verified successfully",
	})
}

// HandleCaptchaStatus 处理验证码状态查询
func HandleCaptchaStatus(w http.ResponseWriter, r *http.Request) {
	captchaParamLock.RLock()
	hasParam := cachedCaptchaParam != "" && time.Now().Before(captchaParamExpiry)
	remaining := time.Until(captchaParamExpiry).Minutes()
	captchaParamLock.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"has_valid_captcha": hasParam,
		"remaining_minutes": int(remaining),
	})
}
