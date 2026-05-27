package main

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"time"

	"zai-proxy/internal"
	bp "zai-proxy/internal/browserproxy"
)

func main() {
	internal.LoadConfig()
	internal.InitLogger()
	internal.StartVersionUpdater()

	// Token manager
	_ = internal.GetTokenManager()

	// ── Browser proxy pool (Go chromedp) ──
	poolSize := 10
	if v := os.Getenv("BROWSER_POOL_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			poolSize = n
		}
	}

	go func() {
		if err := bp.Init(poolSize); err != nil {
			internal.LogError("[BrowserPool] Init failed: %v (anonymous mode disabled)", err)
		}
	}()

	// ── Routes ──
	http.HandleFunc("/v1/models", internal.HandleModels)
	http.HandleFunc("/v1/chat/completions", internal.HandleChatCompletions)
	http.HandleFunc("/v1/messages", internal.HandleAnthropicMessages)
	http.HandleFunc("/v1/responses", internal.HandleResponses)

	// Token management
	http.HandleFunc("/v1/tokens", internal.HandleTokenList)
	http.HandleFunc("/v1/tokens/add", internal.HandleTokenAdd)
	http.HandleFunc("/v1/tokens/stats", internal.HandleTokenStats)

	// Captcha
	http.HandleFunc("/captcha", internal.HandleCaptchaPage)
	http.HandleFunc("/captcha/verify", internal.HandleCaptchaVerify)
	http.HandleFunc("/captcha/status", internal.HandleCaptchaStatus)

	// Browser pool status
	http.HandleFunc("/browser/status", bp.HandleStatus)

	addr := ":" + internal.Cfg.Port
	internal.LogInfo("Server starting on %s", addr)
	internal.LogInfo("Platform: %s/%s", runtime.GOOS, runtime.GOARCH)
	internal.LogInfo("GOMAXPROCS: %d", runtime.GOMAXPROCS(0))
	internal.LogInfo("Browser pool size: %d", poolSize)

	server := &http.Server{
		Addr:              addr,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       300 * time.Second,
		WriteTimeout:      300 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20, // 1 MB
	}

	if err := server.ListenAndServe(); err != nil {
		internal.LogError("Server failed: %v", err)
	}
}
