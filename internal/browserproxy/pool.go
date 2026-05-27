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

// Session holds a token + cookies from a browser session.
type Session struct {
	Token              string
	Cookies            string
	CaptchaVerifyParam string
	Created            time.Time
}

// ───────── Session Pool ─────────

var (
	poolCtx        context.Context
	poolCancel     context.CancelFunc
	sessions       []*Session
	sessionsMu     sync.RWMutex
	poolReady      atomic.Bool
	activeSessions atomic.Int32
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
			s, err := fetchSession(id)
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

func fetchSession(id int) (*Session, error) {
	ctx, cancel := chromedp.NewContext(poolCtx)
	defer cancel()

	navCtx, navCancel := context.WithTimeout(ctx, navigateTimeout)
	defer navCancel()

	var token string
	var cookies string

	err := chromedp.Run(navCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(5*time.Second),
		chromedp.Evaluate(`localStorage.getItem('token') || ''`, &token),
		chromedp.Evaluate(`document.cookie`, &cookies),
	)
	if err != nil {
		return nil, fmt.Errorf("navigate: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("no token found in localStorage")
	}

	// Attempt captcha solve
	captchaParam := ""
	if err := solveCaptcha(ctx); err != nil {
		log.Printf("[BrowserPool] S%d captcha skipped: %v", id, err)
	} else if p := getCachedCaptchaParam(); p != "" {
		captchaParam = p
	}

	log.Printf("[BrowserPool] S%d ready: token=%s..., cookies=%d, captcha=%v",
		id, token[:min(30, len(token))], len(cookies), captchaParam != "")

	return &Session{
		Token:              token,
		Cookies:            cookies,
		CaptchaVerifyParam: captchaParam,
		Created:            time.Now(),
	}, nil
}

func refreshLoop() {
	ticker := time.NewTicker(25 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		log.Printf("[BrowserPool] Refreshing sessions...")
		newSessions := make([]*Session, 0, len(sessions))
		for i := range sessions {
			s, err := fetchSession(i)
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
	for _, s := range sessions {
		if s.CaptchaVerifyParam != "" && time.Since(s.Created) < 25*time.Minute {
			return s
		}
	}
	for _, s := range sessions {
		if time.Since(s.Created) < 25*time.Minute {
			return s
		}
	}
	return sessions[0]
}

func PoolSize() int      { return int(activeSessions.Load()) }
func IsReady() bool      { return poolReady.Load() }

func HandleStatus(w http.ResponseWriter, _ *http.Request) {
	sessionsMu.RLock()
	defer sessionsMu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ready":       poolReady.Load(),
		"sessions":    len(sessions),
		"active":      activeSessions.Load(),
		"has_captcha": hasCaptchaSession(),
	})
}

func hasCaptchaSession() bool {
	for _, s := range sessions {
		if s.CaptchaVerifyParam != "" && time.Since(s.Created) < 25*time.Minute {
			return true
		}
	}
	return false
}

// ───────── Captcha Solver ─────────

var (
	cachedCaptchaParam string
	captchaParamMu     sync.RWMutex
	captchaParamExpiry time.Time
)

func getCachedCaptchaParam() string {
	captchaParamMu.RLock()
	defer captchaParamMu.RUnlock()
	if cachedCaptchaParam != "" && time.Now().Before(captchaParamExpiry) {
		return cachedCaptchaParam
	}
	return ""
}

func setCachedCaptchaParam(param string) {
	captchaParamMu.Lock()
	defer captchaParamMu.Unlock()
	cachedCaptchaParam = param
	captchaParamExpiry = time.Now().Add(30 * time.Minute)
}

func solveCaptcha(ctx context.Context) error {
	// Enable network interception to capture captcha verification
	chromedp.Run(ctx, network.Enable())

	captchaCtx, cancel := chromedp.NewContext(ctx)
	defer cancel()

	navCtx, navCancel := context.WithTimeout(captchaCtx, 30*time.Second)
	defer navCancel()

	err := chromedp.Run(navCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second),
	)
	if err != nil {
		return fmt.Errorf("navigate: %w", err)
	}

	// Check if captcha is present
	var hasCaptcha bool
	chromedp.Run(captchaCtx, chromedp.Evaluate(`
		!!(document.querySelector('#aliyunCaptcha') || 
		   document.querySelector('[data-aliyun-captcha]') ||
		   document.querySelector('.captcha-container') ||
		   document.querySelector('#nc_1_wrapper') ||
		   document.querySelector('.nc-container'))
	`, &hasCaptcha))

	if !hasCaptcha {
		log.Printf("[Captcha] No captcha detected, session pre-authenticated")
		return nil
	}

	log.Printf("[Captcha] Captcha detected, attempting to solve...")

	// Find slider element
	var sliderInfo string
	findCtx, findCancel := context.WithTimeout(captchaCtx, 10*time.Second)
	err = chromedp.Run(findCtx, chromedp.Evaluate(`
		(function() {
			var selectors = [
				'#nc_1_n1z', '.nc_iconfont.btn_slide', '.slider',
				'.btn_slide', '#nc_1__scale_text', '.nc-container .btn_slide',
				'div[class*="slider"]', 'div[class*="slide"]', 'span[class*="nc"]'
			];
			for (var i = 0; i < selectors.length; i++) {
				var el = document.querySelector(selectors[i]);
				if (el) {
					var rect = el.getBoundingClientRect();
					return JSON.stringify({
						found: true, selector: selectors[i],
						x: rect.x, y: rect.y,
						width: rect.width, height: rect.height
					});
				}
			}
			var allElements = document.querySelectorAll('*');
			for (var j = 0; j < allElements.length; j++) {
				var elem = allElements[j];
				var style = window.getComputedStyle(elem);
				if (style.cursor === 'pointer' && 
					(elem.className.toString().includes('slide') || 
					 elem.className.toString().includes('drag') ||
					 elem.className.toString().includes('nc_'))) {
					var rect = elem.getBoundingClientRect();
					if (rect.width > 20 && rect.width < 100 && rect.height > 20 && rect.height < 100) {
						return JSON.stringify({
							found: true, selector: 'dynamic:' + elem.className,
							x: rect.x, y: rect.y,
							width: rect.width, height: rect.height
						});
					}
				}
			}
			return JSON.stringify({found: false});
		})()
	`, &sliderInfo))
	findCancel()
	if err != nil {
		return fmt.Errorf("find slider: %w", err)
	}

	var slider map[string]interface{}
	json.Unmarshal([]byte(sliderInfo), &slider)

	if found, ok := slider["found"].(bool); !ok || !found {
		return solveCaptchaAlternative(captchaCtx)
	}

	log.Printf("[Captcha] Found slider: %v", slider)

	// Find gap position
	var gapInfo string
	gapCtx, gapCancel := context.WithTimeout(captchaCtx, 10*time.Second)
	err = chromedp.Run(gapCtx, chromedp.Evaluate(`
		(function() {
			var bgSelectors = [
				'.bg-img', '.captcha-bg', '.puzzle-bg', 
				'#nc_1__imgCaptcha', '.img-captcha',
				'div[class*="bg"]', 'canvas'
			];
			for (var i = 0; i < bgSelectors.length; i++) {
				var el = document.querySelector(bgSelectors[i]);
				if (el) {
					var rect = el.getBoundingClientRect();
					var style = window.getComputedStyle(el);
					var bgImage = style.backgroundImage;
					if (bgImage && bgImage !== 'none') {
						return JSON.stringify({
							found: true, element: bgSelectors[i],
							x: rect.x, y: rect.y,
							width: rect.width, height: rect.height,
							bgImage: bgImage.substring(0, 100)
						});
					}
					if (el.tagName === 'CANVAS') {
						return JSON.stringify({
							found: true, element: bgSelectors[i],
							x: rect.x, y: rect.y,
							width: rect.width, height: rect.height,
							isCanvas: true
						});
					}
				}
			}
			var allDivs = document.querySelectorAll('div, img, canvas');
			for (var j = 0; j < allDivs.length; j++) {
				var elem = allDivs[j];
				var cls = (elem.className || '').toString();
				if (cls.includes('captcha') || cls.includes('puzzle') || cls.includes('slide')) {
					var rect = elem.getBoundingClientRect();
					if (rect.width > 100 && rect.height > 50) {
						return JSON.stringify({
							found: true, element: 'dynamic:' + cls,
							x: rect.x, y: rect.y,
							width: rect.width, height: rect.height
						});
					}
				}
			}
			return JSON.stringify({found: false});
		})()
	`, &gapInfo))
	gapCancel()
	if err != nil {
		return fmt.Errorf("find gap: %w", err)
	}

	var gap map[string]interface{}
	json.Unmarshal([]byte(gapInfo), &gap)
	log.Printf("[Captcha] Gap info: %v", gap)

	// Calculate slide parameters
	sliderX, _ := slider["x"].(float64)
	sliderY, _ := slider["y"].(float64)
	sliderW, _ := slider["width"].(float64)
	sliderH, _ := slider["height"].(float64)

	startX := sliderX + sliderW/2
	startY := sliderY + sliderH/2

	var slideDistance float64 = 200
	if found, ok := gap["found"].(bool); ok && found {
		gapW, _ := gap["width"].(float64)
		if gapW > 0 {
			slideDistance = gapW * 0.6
		}
	}
	endX := startX + slideDistance

	log.Printf("[Captcha] Sliding (%.1f,%.1f) -> (%.1f,%.1f)", startX, startY, endX, startY)

	// Perform slide via JavaScript mouse events
	slideCtx, slideCancel := context.WithTimeout(captchaCtx, 15*time.Second)
	var slideOK bool
	err = chromedp.Run(slideCtx, chromedp.Evaluate(fmt.Sprintf(`
		(function() {
			var startX = %.1f, startY = %.1f, endX = %.1f;
			var steps = 30;
			function mouseEvent(type, x, y) {
				var el = document.elementFromPoint(x, y) || document.body;
				el.dispatchEvent(new MouseEvent(type, {
					bubbles: true, cancelable: true,
					clientX: x, clientY: y, button: 0,
					buttons: type === 'mouseup' ? 0 : 1
				}));
			}
			var points = [];
			for (var i = 0; i <= steps; i++) {
				var p = i / steps;
				var ease = p < 0.5 ? 2*p*p : 1 - Math.pow(-2*p+2,2)/2;
				points.push({
					x: startX + (endX-startX)*ease,
					y: startY + (Math.random()-0.5)*3
				});
			}
			mouseEvent('mousedown', startX, startY);
			for (var j = 0; j < points.length; j++) {
				(function(pt, d){
					setTimeout(function(){ mouseEvent('mousemove', pt.x, pt.y); }, d);
				})(points[j], j*20);
			}
			setTimeout(function(){ mouseEvent('mouseup', endX, startY); }, (points.length+1)*20);
			return true;
		})()
	`, startX, startY, endX), &slideOK))
	slideCancel()
	if err != nil {
		return fmt.Errorf("slide: %w", err)
	}

	time.Sleep(3 * time.Second)

	// Check if solved
	var solved bool
	chromedp.Run(captchaCtx, chromedp.Evaluate(`
		!!(document.querySelector('.captcha-success') || 
		   document.querySelector('[data-captcha-success]') ||
		   !document.querySelector('#aliyunCaptcha'))
	`, &solved))

	if solved {
		log.Printf("[Captcha] Captcha solved!")
		var param string
		chromedp.Run(captchaCtx, chromedp.Evaluate(`window.__captcha_verify_param || ''`, &param))
		if param != "" {
			setCachedCaptchaParam(param)
		}
		return nil
	}

	return solveCaptchaAlternative(captchaCtx)
}

func solveCaptchaAlternative(ctx context.Context) error {
	var hasClickCaptcha bool
	chromedp.Run(ctx, chromedp.Evaluate(`
		!!(document.querySelector('.click-captcha') || 
		   document.querySelector('.verify-btn') ||
		   document.querySelector('.captcha-verify-btn'))
	`, &hasClickCaptcha))

	if hasClickCaptcha {
		log.Printf("[Captcha] Trying click captcha...")
		clickCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := chromedp.Run(clickCtx,
			chromedp.Click(".click-captcha, .verify-btn, .captcha-verify-btn", chromedp.ByQuery),
			chromedp.Sleep(3*time.Second),
		)
		cancel()
		if err == nil {
			log.Printf("[Captcha] Click captcha solved!")
			return nil
		}
	}

	log.Printf("[Captcha] Automated solving failed, will use browser chat fallback")
	return fmt.Errorf("captcha solving not fully automated")
}

// ───────── Chat via Browser ─────────

func ChatViaBrowser(userMessage string) (string, error) {
	if !poolReady.Load() {
		return "", fmt.Errorf("browser pool not ready")
	}

	ctx, cancel := chromedp.NewContext(poolCtx)
	defer cancel()

	chatCtx, chatCancel := context.WithTimeout(ctx, chatTimeout)
	defer chatCancel()

	err := chromedp.Run(chatCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(3*time.Second),
	)
	if err != nil {
		return "", fmt.Errorf("navigate: %w", err)
	}

	// Find and type message using JavaScript
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
				input.click();
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
			var btns = document.querySelectorAll(
				'button[class*="send"], button[class*="submit"], ' +
				'[class*="send-btn"], button[aria-label*="send"], button[aria-label*="Send"]'
			);
			for (var i = 0; i < btns.length; i++) {
				if (btns[i].offsetParent !== null) { btns[i].click(); return 'clicked'; }
			}
			return 'enter';
		})()
	`, &sendResult))
	sendCancel()

	log.Printf("[BrowserChat] Send: %s", sendResult)

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
