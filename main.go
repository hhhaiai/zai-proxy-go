package main

import (
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"time"

	"zai-proxy/internal"
)

var browserProxyCmd *exec.Cmd

// startBrowserProxy launches the browser proxy as a subprocess
func startBrowserProxy() {
	// Find browser_proxy.js
	scriptPath := findScript("browser_proxy.js")
	if scriptPath == "" {
		internal.LogWarn("browser_proxy.js not found, anonymous mode will not work")
		return
	}

	internal.LogInfo("Starting browser proxy from %s", scriptPath)

	browserProxyCmd = exec.Command("node", scriptPath)
	browserProxyCmd.Dir = filepath.Dir(scriptPath)
	browserProxyCmd.Stdout = os.Stdout
	browserProxyCmd.Stderr = os.Stderr

	// Set pool size (default 5, can override with BROWSER_POOL_SIZE env)
	if os.Getenv("BROWSER_POOL_SIZE") == "" {
		browserProxyCmd.Env = append(os.Environ(), "BROWSER_POOL_SIZE=5")
	}

	if err := browserProxyCmd.Start(); err != nil {
		internal.LogError("Failed to start browser proxy: %v", err)
		return
	}

	internal.LogInfo("Browser proxy started (PID: %d)", browserProxyCmd.Process.Pid)

	// Monitor process
	go func() {
		if err := browserProxyCmd.Wait(); err != nil {
			internal.LogError("Browser proxy exited: %v", err)
		}
	}()
}

// waitForBrowserProxy waits until the browser proxy is ready
func waitForBrowserProxy() {
	internal.LogInfo("Waiting for browser proxy to be ready...")
	client := &http.Client{Timeout: 2 * time.Second}

	for i := 0; i < 60; i++ { // Wait up to 60 seconds
		resp, err := client.Get("http://localhost:9877/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				internal.LogInfo("Browser proxy is ready!")
				return
			}
		}
		time.Sleep(1 * time.Second)
		if i%10 == 9 {
			internal.LogInfo("Still waiting for browser proxy... (%ds)", i+1)
		}
	}
	internal.LogWarn("Browser proxy did not become ready in 60s")
}

func findScript(name string) string {
	// Check current directory
	if _, err := os.Stat(name); err == nil {
		abs, _ := filepath.Abs(name)
		return abs
	}

	// Check executable directory
	execPath, err := os.Executable()
	if err == nil {
		dir := filepath.Dir(execPath)
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}

	// Check common locations
	wd, _ := os.Getwd()
	candidates := []string{
		filepath.Join(wd, name),
		filepath.Join(wd, "..", name),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			abs, _ := filepath.Abs(c)
			return abs
		}
	}

	return ""
}

func main() {
	internal.LoadConfig()
	internal.InitLogger()
	internal.StartVersionUpdater()

	// 初始化Token管理器
	_ = internal.GetTokenManager()

	http.HandleFunc("/v1/models", internal.HandleModels)
	http.HandleFunc("/v1/chat/completions", internal.HandleChatCompletions)
	http.HandleFunc("/v1/messages", internal.HandleAnthropicMessages)

	// Token管理相关API
	http.HandleFunc("/v1/tokens", internal.HandleTokenList)
	http.HandleFunc("/v1/tokens/add", internal.HandleTokenAdd)
	http.HandleFunc("/v1/tokens/stats", internal.HandleTokenStats)

	// Captcha debugging endpoints
	http.HandleFunc("/captcha", internal.HandleCaptchaPage)
	http.HandleFunc("/captcha/verify", internal.HandleCaptchaVerify)
	http.HandleFunc("/captcha/status", internal.HandleCaptchaStatus)

	addr := ":" + internal.Cfg.Port

	internal.LogInfo("Server starting on %s", addr)
	internal.LogInfo("Platform: %s/%s", runtime.GOOS, runtime.GOARCH)

	// Start browser proxy for anonymous mode
	go func() {
		time.Sleep(1 * time.Second)
		startBrowserProxy()
		waitForBrowserProxy()
	}()

	if err := http.ListenAndServe(addr, nil); err != nil {
		internal.LogError("Server failed: %v", err)
	}
}
