package browserproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"log"
)

const (
	zaiURL          = "https://chat.z.ai"
	defaultPoolSize = 2
	navigateTimeout = 60 * time.Second
	chatTimeout     = 120 * time.Second
)

// Session holds a persistent browser tab for fast message processing.
type Session struct {
	Token   string
	Cookies string
	Created time.Time
	tabCtx  context.Context
	tabMu   sync.Mutex
}

// CaptchaInfo holds the intercepted captcha parameters.
type CaptchaInfo struct {
	CaptchaVerifyParam string
	CapturedAt         time.Time
}

// ───────── Session Pool ─────────

var (
	poolCtx        context.Context
	poolCancel     context.CancelFunc
	sessions       []*Session
	sessionsMu     sync.RWMutex
	poolReady      atomic.Bool
	activeSessions atomic.Int32
	sessionIdx     atomic.Int64
	
	// Captcha cache
	cachedCaptcha     *CaptchaInfo
	captchaMu         sync.RWMutex
)

func Init(poolSize int) error {
	if poolSize <= 0 {
		poolSize = defaultPoolSize
	}

	log.Printf("[BrowserPool] Starting chromedp (sessions=%d, %s/%s)", poolSize, runtime.GOOS, runtime.GOARCH)

	chromePath := findChrome()
	if chromePath == "" {
		return fmt.Errorf("Chrome/Chromium not found – install chromium or set CHROME_PATH")
	}
	log.Printf("[BrowserPool] Chrome: %s", chromePath)

	opts := append(chromedp.DefaultExecAllocatorOptions[:],
		chromedp.ExecPath(chromePath),
		chromedp.Flag("headless", true),
		chromedp.Flag("no-sandbox", true),
		chromedp.Flag("disable-gpu", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-blink-features", "AutomationControlled"),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("no-first-run", true),
		chromedp.Flag("mute-audio", true),
		chromedp.WindowSize(1280, 720),
		chromedp.UserAgent("Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	poolCtx, poolCancel = chromedp.NewContext(allocCtx)

	if err := chromedp.Run(poolCtx); err != nil {
		allocCancel()
		return fmt.Errorf("browser launch: %w", err)
	}
	orig := poolCancel
	poolCancel = func() { orig(); allocCancel() }

	sessions = make([]*Session, 0, poolSize)

	var wg sync.WaitGroup
	var mu sync.Mutex
	errCnt := 0

	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			s, err := createSession(id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("[BrowserPool] S%d failed: %v", id, err)
				errCnt++
				return
			}
			sessions = append(sessions, s)
			activeSessions.Add(1)
		}(i)
	}
	wg.Wait()

	if len(sessions) == 0 {
		return fmt.Errorf("all %d sessions failed", poolSize)
	}

	poolReady.Store(true)
	log.Printf("[BrowserPool] Ready: %d/%d sessions", len(sessions), poolSize)
	go refreshLoop()
	return nil
}

func createSession(id int) (*Session, error) {
	tabCtx, cancel := chromedp.NewContext(poolCtx)
	defer cancel()

	navCtx, navCancel := context.WithTimeout(tabCtx, navigateTimeout)
	defer navCancel()

	var token string
	var cookies string

	err := chromedp.Run(navCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(8*time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}

	// Debug: check what's in localStorage and cookies
	var debugInfo string
	chromedp.Run(navCtx, chromedp.Evaluate(`
		JSON.stringify({
			localStorageKeys: Object.keys(localStorage),
			token: localStorage.getItem('token'),
			cookie: document.cookie.substring(0, 200),
			url: window.location.href,
			title: document.title
		})
	`, &debugInfo))
	log.Printf("[BrowserPool] S%d debug: %s", id, debugInfo)

	// Try to get token from localStorage
	chromedp.Run(navCtx, chromedp.Evaluate(`localStorage.getItem('token') || ''`, &token))
	chromedp.Run(navCtx, chromedp.Evaluate(`document.cookie`, &cookies))

	// If no token in localStorage, try to get from cookies
	if token == "" {
		// Parse token from cookies
		cookieArr := strings.Split(cookies, ";")
		for _, c := range cookieArr {
			c = strings.TrimSpace(c)
			if strings.HasPrefix(c, "token=") {
				token = strings.TrimPrefix(c, "token=")
				break
			}
		}
	}

	// If still no token, try to get from page context
	if token == "" {
		chromedp.Run(navCtx, chromedp.Evaluate(`
			// Try various token locations
			window.__token || 
			document.querySelector('meta[name="token"]')?.content ||
			document.querySelector('input[name="token"]')?.value ||
			''
		`, &token))
	}

	if token == "" {
		return nil, fmt.Errorf("no token found in localStorage, cookies, or page context")
	}

	log.Printf("[BrowserPool] S%d ready: token=%s..., cookies=%d", id, token[:min(30, len(token))], len(cookies))

	return &Session{
		Token:   token,
		Cookies: cookies,
		Created: time.Now(),
		tabCtx:  tabCtx,
	}, nil
}

func refreshLoop() {
	ticker := time.NewTicker(25 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		log.Printf("[BrowserPool] Refreshing sessions...")
		newSessions := make([]*Session, 0, len(sessions))
		for i := range sessions {
			s, err := createSession(i)
			if err != nil {
				log.Printf("[BrowserPool] S%d refresh failed: %v", i, err)
				continue
			}
			newSessions = append(newSessions, s)
		}
		if len(newSessions) > 0 {
			sessionsMu.Lock()
			sessions = newSessions
			sessionsMu.Unlock()
			log.Printf("[BrowserPool] Refreshed: %d sessions", len(newSessions))
		}
	}
}

// ───────── Public API ─────────

func GetSession() *Session {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	if len(sessions) == 0 {
		return nil
	}
	idx := int(sessionIdx.Add(1)) % len(sessions)
	return sessions[idx]
}

func PoolSize() int      { return int(activeSessions.Load()) }
func IsReady() bool      { return poolReady.Load() }

func HandleStatus(w http.ResponseWriter, _ *http.Request) {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	
	captchaMu.RLock()
	hasCaptcha := cachedCaptcha != nil && time.Since(cachedCaptcha.CapturedAt) < 30*time.Minute
	captchaMu.RUnlock()
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ready":       poolReady.Load(),
		"sessions":    len(sessions),
		"active":      activeSessions.Load(),
		"has_captcha": hasCaptcha,
	})
}

// GetCachedCaptcha returns the cached captcha_verify_param if valid.
func GetCachedCaptcha() string {
	captchaMu.RLock()
	defer captchaMu.RUnlock()
	if cachedCaptcha != nil && time.Since(cachedCaptcha.CapturedAt) < 30*time.Minute {
		return cachedCaptcha.CaptchaVerifyParam
	}
	return ""
}

// SetCachedCaptcha caches the captcha_verify_param.
func SetCachedCaptcha(param string) {
	captchaMu.Lock()
	defer captchaMu.Unlock()
	cachedCaptcha = &CaptchaInfo{
		CaptchaVerifyParam: param,
		CapturedAt:         time.Now(),
	}
	log.Printf("[Captcha] Captured captcha_verify_param (len=%d)", len(param))
}

// ───────── Chat via Browser with Network Interception ─────────

func ChatViaBrowser(userMessage string) (string, error) {
	if !poolReady.Load() {
		return "", fmt.Errorf("browser pool not ready")
	}

	// Create a new tab for this request
	tabCtx, cancel := chromedp.NewContext(poolCtx)
	defer cancel()

	chatCtx, chatCancel := context.WithTimeout(tabCtx, chatTimeout)
	defer chatCancel()

	// Enable network interception
	chromedp.Run(chatCtx, network.Enable())

	// Set up network event listener to capture captcha_verify_param
	var capturedCaptchaParam string
	
	// Listen for network events using chromedp's event system
	lctx, lcancel := context.WithCancel(chatCtx)
	defer lcancel()
	
	chromedp.Run(lctx, chromedp.ActionFunc(func(ctx context.Context) error {
		// This is a placeholder - the actual interception happens via JavaScript
		return nil
	}))

	// Navigate to z.ai
	err := chromedp.Run(chatCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second),
	)
	if err != nil {
		return "", fmt.Errorf("navigate: %w", err)
	}

	// Inject JavaScript to intercept fetch/XHR requests and capture captcha_verify_param
	chromedp.Run(chatCtx, chromedp.Evaluate(`
		(function() {
			// Intercept fetch requests
			const originalFetch = window.fetch;
			window.fetch = function(...args) {
				const url = args[0];
				const options = args[1] || {};
				
				if (url && url.includes('chat.z.ai/api') && options.method === 'POST') {
					try {
						const body = JSON.parse(options.body);
						if (body.captcha_verify_param) {
							window.__intercepted_captcha = body.captcha_verify_param;
							console.log('[Intercept] Captured captcha_verify_param');
						}
					} catch(e) {}
				}
				
				return originalFetch.apply(this, args);
			};
			
			// Intercept XMLHttpRequest
			const originalOpen = XMLHttpRequest.prototype.open;
			const originalSend = XMLHttpRequest.prototype.send;
			
			XMLHttpRequest.prototype.open = function(method, url) {
				this._url = url;
				this._method = method;
				return originalOpen.apply(this, arguments);
			};
			
			XMLHttpRequest.prototype.send = function(body) {
				if (this._url && this._url.includes('chat.z.ai/api') && this._method === 'POST') {
					try {
						const parsed = JSON.parse(body);
						if (parsed.captcha_verify_param) {
							window.__intercepted_captcha = parsed.captcha_verify_param;
							console.log('[Intercept] Captured captcha_verify_param via XHR');
						}
					} catch(e) {}
				}
				return originalSend.apply(this, arguments);
			};
			
			console.log('[Intercept] Network interception installed');
		})()
	`, nil))

	// Find and type message
	var typeResult string
	typeCtx, typeCancel := context.WithTimeout(chatCtx, 15*time.Second)
	err = chromedp.Run(typeCtx,
		chromedp.Sleep(3*time.Second),
		chromedp.Evaluate(fmt.Sprintf(`
			(function() {
				var msg = %q;
				var input = document.querySelector('textarea') || 
				            document.querySelector('div[contenteditable="true"]') ||
				            document.querySelector('input[type="text"]');
				if (!input) {
					var allInputs = document.querySelectorAll('textarea, input, [contenteditable]');
					for (var i = 0; i < allInputs.length; i++) {
						var el = allInputs[i];
						var rect = el.getBoundingClientRect();
						if (rect.width > 50 && rect.height > 20 && rect.top > 0) {
							input = el;
							break;
						}
					}
				}
				if (!input) return 'no-input-found';
				if (input.tagName === 'TEXTAREA' || input.tagName === 'INPUT') {
					input.value = msg;
					input.dispatchEvent(new Event('input', {bubbles: true}));
					input.dispatchEvent(new Event('change', {bubbles: true}));
				} else if (input.contentEditable === 'true') {
					input.textContent = msg;
					input.dispatchEvent(new Event('input', {bubbles: true}));
				}
				input.focus();
				return 'typed';
			})()
		`, userMessage), &typeResult),
	)
	typeCancel()
	if err != nil {
		return "", fmt.Errorf("type: %w", err)
	}
	if typeResult != "typed" {
		return "", fmt.Errorf("could not type: %s", typeResult)
	}

	// Send message
	var sendResult string
	sendCtx, sendCancel := context.WithTimeout(chatCtx, 5*time.Second)
	chromedp.Run(sendCtx, chromedp.Evaluate(`
		(function() {
			var input = document.querySelector('textarea') || 
			            document.querySelector('div[contenteditable="true"]') ||
			            document.querySelector('input[type="text"]');
			if (!input) return 'no-input';
			input.dispatchEvent(new KeyboardEvent('keydown', {
				key:'Enter', code:'Enter', keyCode:13, which:13,
				bubbles:true, cancelable:true
			}));
			var btns = document.querySelectorAll('button');
			for (var i = 0; i < btns.length; i++) {
				var btn = btns[i];
				var text = (btn.textContent || '').toLowerCase();
				var ariaLabel = (btn.getAttribute('aria-label') || '').toLowerCase();
				if (text.includes('send') || text.includes('发送') || 
				    ariaLabel.includes('send') || ariaLabel.includes('发送') ||
				    btn.querySelector('svg')) {
					btn.click();
					return 'clicked-send';
				}
			}
			return 'enter';
		})()
	`, &sendResult))
	sendCancel()

	log.Printf("[BrowserChat] Send: %s", sendResult)

	// Wait for the request to be sent and intercept captcha_verify_param
	time.Sleep(3 * time.Second)

	// Debug: check if interception was installed and if captcha was captured
	var debugInfo string
	chromedp.Run(chatCtx, chromedp.Evaluate(`
		JSON.stringify({
			interceptionInstalled: typeof window.fetch.toString().includes('chat.z.ai') || true,
			hasIntercepted: !!window.__intercepted_captcha,
			interceptedValue: window.__intercepted_captcha || 'none',
			fetchOverwritten: window.fetch.toString().substring(0, 100)
		})
	`, &debugInfo))
	log.Printf("[BrowserChat] Debug: %s", debugInfo)

	// Check if we intercepted captcha_verify_param
	chromedp.Run(chatCtx, chromedp.Evaluate(`
		if (window.__intercepted_captcha) {
			window.__captcha_result = window.__intercepted_captcha;
		}
	`, nil))

	var interceptedParam string
	chromedp.Run(chatCtx, chromedp.Evaluate(`window.__captcha_result || ''`, &interceptedParam))
	
	if interceptedParam != "" {
		capturedCaptchaParam = interceptedParam
		SetCachedCaptcha(capturedCaptchaParam)
		log.Printf("[BrowserChat] Captured captcha_verify_param (len=%d)", len(capturedCaptchaParam))
	}

	// Poll for response
	var prevLen int
	stableCount := 0
	startTime := time.Now()

	for i := 0; i < 240; i++ {
		time.Sleep(500 * time.Millisecond)
		if time.Since(startTime) > chatTimeout {
			return "", fmt.Errorf("timeout")
		}

		var text string
		pollCtx, pollCancel := context.WithTimeout(chatCtx, 3*time.Second)
		chromedp.Run(pollCtx, chromedp.Evaluate(`document.body.innerText`, &text))
		pollCancel()

		curLen := len(text)
		if curLen == prevLen {
			stableCount++
			if stableCount >= 8 {
				return extractAnswerFromText(text, userMessage)
			}
		} else {
			stableCount = 0
		}
		prevLen = curLen
	}
	return "", fmt.Errorf("timeout waiting for response")
}

func extractAnswerFromText(pageText, userMsg string) (string, error) {
	lines := strings.Split(pageText, "\n")
	var answerLines []string
	found := false
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == userMsg { found = true; continue }
		if found {
			lower := strings.ToLower(line)
			if strings.Contains(lower, "以上内容均由ai生成") ||
				strings.Contains(lower, "技术博客") ||
				strings.Contains(lower, "联系我们") ||
				strings.Contains(lower, "用户协议") ||
				strings.Contains(lower, "隐私政策") ||
				strings.Contains(lower, "有什么我能帮") {
				break
			}
			if line == "Regenerate" || line == "Copy" || line == "Like" || line == "Dislike" {
				continue
			}
			answerLines = append(answerLines, line)
		}
	}
	answer := strings.TrimSpace(strings.Join(answerLines, "\n"))
	if answer == "" {
		return "", fmt.Errorf("no answer extracted")
	}
	rawLines := strings.Split(answer, "\n")
	if len(rawLines) > 1 {
		return strings.TrimSpace(rawLines[len(rawLines)-1]), nil
	}
	return answer, nil
}

// ───────── Chrome Discovery ─────────

func findChrome() string {
	if p := os.Getenv("CHROME_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil { return p }
	}
	switch runtime.GOOS {
	case "darwin":
		for _, c := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if _, err := os.Stat(c); err == nil { return c }
		}
	case "linux":
		for _, c := range []string{"chromium-browser", "chromium", "google-chrome", "google-chrome-stable"} {
			if p, err := exec.LookPath(c); err == nil { return p }
		}
	case "windows":
		for _, c := range []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		} {
			if _, err := os.Stat(c); err == nil { return c }
		}
	}
	for _, c := range []string{"google-chrome", "chromium"} {
		if p, err := exec.LookPath(c); err == nil { return p }
	}
	return ""
}

// ───────── Compatibility stubs ─────────

func Acquire() *worker   { return nil }
func Release(w *worker)  {}
func ProcessChat(w *worker, userMessage string) (string, error) {
	return ChatViaBrowser(userMessage)
}

type worker struct{}
