package internal

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

// PuppeteerPool manages a reusable Puppeteer browser session.
// Instead of launching a new browser per request, we keep one alive
// and send messages through it via stdin/stdout protocol.
var (
	puppeteerOnce sync.Once
	puppeteerMu   sync.Mutex
)

// RunPuppeteerChat sends a message through the captcha_solver.js and returns the answer.
func RunPuppeteerChat(userMsg string) (string, string, error) {
	puppeteerMu.Lock()
	defer puppeteerMu.Unlock()

	scriptPath := findPuppeteerScript()
	if scriptPath == "" {
		return "", "", fmt.Errorf("captcha_solver.js not found")
	}

	ctx, cancel := timeoutContext(120 * time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "node", scriptPath, "--msg="+userMsg)
	cmd.Dir = filepath.Dir(scriptPath)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", "", fmt.Errorf("stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return "", "", fmt.Errorf("stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("start: %w", err)
	}

	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := stderr.Read(buf)
			if n > 0 {
				LogDebug("[Puppeteer] %s", string(buf[:n]))
			}
			if err != nil {
				break
			}
		}
	}()

	stdoutBytes, _ := io.ReadAll(stdout)
	waitErr := cmd.Wait()

	var result struct {
		Token     string `json:"token"`
		Answer    string `json:"answer"`
		Cookies   string `json:"cookies"`
		HasToken  bool   `json:"hasToken"`
		HasAnswer bool   `json:"hasAnswer"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(stdoutBytes, &result); err != nil {
		return "", "", fmt.Errorf("parse: %w (wait=%v)", err, waitErr)
	}
	if result.Error != "" {
		return "", "", fmt.Errorf("puppeteer: %s", result.Error)
	}
	if !result.HasAnswer {
		return "", "", fmt.Errorf("no answer from puppeteer")
	}

	return result.Answer, result.Token, nil
}

func findPuppeteerScript() string {
	cwd, _ := os.Getwd()
	candidates := []string{
		filepath.Join(cwd, "captcha_solver.js"),
		filepath.Join(filepath.Dir(os.Args[0]), "captcha_solver.js"),
	}
	for _, p := range candidates {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}
