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
	"time"

	"github.com/chromedp/chromedp"

	"log"
)

const (
	zaiURL          = "https://chat.z.ai"
	defaultPoolSize = 10
	navigateTimeout = 45 * time.Second
	requestTimeout  = 120 * time.Second
)

// ───────── Worker ─────────

type worker struct {
	id     int
	ctx    context.Context
	cancel context.CancelFunc
	busy   bool
	mu     sync.Mutex
}

func (w *worker) markBusy() { w.mu.Lock(); w.busy = true; w.mu.Unlock() }
func (w *worker) markFree() { w.mu.Lock(); w.busy = false; w.mu.Unlock() }

// ensureAlive checks if the page is responsive. If not, recreates the tab context.
func (w *worker) ensureAlive() error {
	var title string
	checkCtx, checkCancel := context.WithTimeout(w.ctx, 20*time.Second)
	err := chromedp.Run(checkCtx, chromedp.Title(&title))
	checkCancel()
	if err == nil {
		log.Printf("[BrowserPool] W%d alive (%s)", w.id, title)
		return nil
	}

	log.Printf("[BrowserPool] W%d dead (%v), recreating tab…", w.id, err)

	// Cancel old context, create new one
	w.cancel()
	w.ctx, w.cancel = chromedp.NewContext(browserCtx)

	navCtx, navCancel := context.WithTimeout(w.ctx, 45*time.Second)
	err = chromedp.Run(navCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(5*time.Second),
	)
	navCancel()
	if err != nil {
		return fmt.Errorf("recreate failed: %w", err)
	}
	log.Printf("[BrowserPool] W%d recreated", w.id)
	return nil
}

// ───────── Pool ─────────

var (
	pool          []*worker
	readyCh       chan *worker
	browserCtx    context.Context
	browserCancel context.CancelFunc
	initOnce      sync.Once
	initErr       error
	poolReady     bool
)

func Init(poolSize int) error {
	initOnce.Do(func() {
		if poolSize <= 0 {
			poolSize = defaultPoolSize
		}
		initErr = doInit(poolSize)
	})
	return initErr
}

func doInit(poolSize int) error {
	log.Printf("[BrowserPool] Starting chromedp (pool=%d, %s/%s)", poolSize, runtime.GOOS, runtime.GOARCH)

	chromePath := findChrome()
	if chromePath == "" {
		return fmt.Errorf("Chrome/Chromium not found – set CHROME_PATH or install Chrome")
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
		chromedp.WindowSize(1280, 720),
		chromedp.UserAgent("Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"),
	)

	allocCtx, allocCancel := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, browserCancel = chromedp.NewContext(allocCtx)

	if err := chromedp.Run(browserCtx); err != nil {
		allocCancel()
		return fmt.Errorf("browser launch: %w", err)
	}
	orig := browserCancel
	browserCancel = func() { orig(); allocCancel() }

	pool = make([]*worker, 0, poolSize)
	readyCh = make(chan *worker, poolSize)

	var wg sync.WaitGroup
	var mu sync.Mutex
	errCnt := 0

	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w, err := createWorker(id)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				log.Printf("[BrowserPool] W%d failed: %v", id, err)
				errCnt++
				return
			}
			pool = append(pool, w)
			readyCh <- w
		}(i)
	}
	wg.Wait()

	if len(pool) == 0 {
		return fmt.Errorf("all %d workers failed", poolSize)
	}

	poolReady = true
	log.Printf("[BrowserPool] Ready: %d/%d workers", len(pool), poolSize)
	return nil
}

func createWorker(id int) (*worker, error) {
	ctx, cancel := chromedp.NewContext(browserCtx)
	navCtx, navCancel := context.WithTimeout(ctx, navigateTimeout)
	defer navCancel()

	if err := chromedp.Run(navCtx,
		chromedp.Navigate(zaiURL),
		chromedp.WaitReady("body"),
		chromedp.Sleep(2*time.Second),
	); err != nil {
		cancel()
		return nil, err
	}
	log.Printf("[BrowserPool] W%d ready", id)
	return &worker{id: id, ctx: ctx, cancel: cancel}, nil
}

func Acquire() *worker   { return <-readyCh }
func Release(w *worker)  { w.markFree(); readyCh <- w }
func PoolSize() int      { return len(pool) }
func IsReady() bool      { return poolReady }

func HandleStatus(w http.ResponseWriter, _ *http.Request) {
	busy := 0
	for _, w2 := range pool {
		if w2.busy {
			busy++
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"ready": poolReady, "pool_size": len(pool),
		"busy": busy, "available": len(pool) - busy,
	})
}

// ───────── Chat via DOM polling ─────────

// ProcessChat submits userMessage through the chat.z.ai UI and returns the
// model's answer. It uses a fresh conversation per call.
//
// Strategy: navigate → find input → type → Enter → poll page text until stable.
func ProcessChat(w *worker, userMessage string) (string, error) {
	w.markBusy()

	log.Printf("[BrowserPool] W%d ProcessChat start", w.id)
	// 1. Fresh conversation
	// Ensure page is alive; if dead, recreate the tab context
	if err := w.ensureAlive(); err != nil {
		return "", fmt.Errorf("worker dead: %w", err)
	}

	// 2. Locate input field
	sel := findInput(w)
	log.Printf("[BrowserPool] W%d input selector: %q", w.id, sel)
	if sel == "" {
		return "", fmt.Errorf("input field not found on chat.z.ai")
	}

	// 3. Type & submit
	typeCtx, typeCancel := context.WithTimeout(w.ctx, 10*time.Second)
	err := chromedp.Run(typeCtx,
		chromedp.Click(sel),
		chromedp.Sleep(300*time.Millisecond),
		chromedp.SendKeys(sel, userMessage+"\n"),
	)
	typeCancel()
	if err != nil {
		log.Printf("[BrowserPool] W%d type err: %v", w.id, err)
		return "", fmt.Errorf("send keys: %w", err)
	}
	log.Printf("[BrowserPool] W%d typed, polling…", w.id)

	// 4. Poll page until response is complete
	answer, err := pollResponse(w)
	log.Printf("[BrowserPool] W%d done: err=%v, len=%d", w.id, err, len(answer))
	return answer, err
}

func findInput(w *worker) string {
	candidates := []string{
		"#chat-input",
		"textarea",
		"[contenteditable='true']",
		"div[role='textbox']",
		"[placeholder]",
	}
	for _, sel := range candidates {
		var n int
		ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
		err := chromedp.Run(ctx,
			chromedp.Evaluate(fmt.Sprintf(`document.querySelectorAll('%s').length`, sel), &n),
		)
		cancel()
		if err == nil && n > 0 {
			log.Printf("[BrowserPool] W%d found input: %s (count=%d)", w.id, sel, n)
			return sel
		}
	}

	// Debug: dump page URL and interactive elements
	var pageURL string
	var html string
	ctx2, cancel2 := context.WithTimeout(w.ctx, 5*time.Second)
	chromedp.Run(ctx2,
		chromedp.Evaluate(`document.location.href`, &pageURL),
		chromedp.Evaluate(`JSON.stringify([...document.querySelectorAll('input,textarea,[contenteditable],[role=textbox],div[class*=input],div[class*=editor],#chat-input')].map(e => ({tag:e.tagName, id:e.id, cls:(e.className||'').substring(0,60), role:e.getAttribute('role'), ce:e.getAttribute('contenteditable')})))`, &html),
	)
	cancel2()
	log.Printf("[BrowserPool] W%d url=%s, DOM elements: %s", w.id, pageURL, html)

	return ""
}

// pollResponse repeatedly reads page textContent until it stabilises
// (no change for 3 consecutive checks → response is complete).
func pollResponse(w *worker) (string, error) {
	deadline := time.After(requestTimeout)
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()

	prevLen := 0
	stableCount := 0
	const stableThreshold = 4 // 4 × 500ms = 2s of no change → done

	for {
		select {
		case <-deadline:
			return "", fmt.Errorf("timeout waiting for response")
		case <-ticker.C:
			var text string
			ctx, cancel := context.WithTimeout(w.ctx, 3*time.Second)
			err := chromedp.Run(ctx,
				chromedp.Evaluate(`document.body.innerText`, &text),
			)
			cancel()
			if err != nil {
				continue
			}

			curLen := len(text)
			if curLen == prevLen {
				stableCount++
				if stableCount >= stableThreshold {
					return extractAnswer(text)
				}
			} else {
				stableCount = 0
			}
			prevLen = curLen
		}
	}
}

// extractAnswer finds the model's response in the full page text.
// chat.z.ai renders the assistant answer after the user message.
// We look for text after the user's input line.
func extractAnswer(pageText string) (string, error) {
	lines := strings.Split(pageText, "\n")

	// Find last non-empty line that looks like a response
	// Skip UI chrome (buttons, nav, footer etc.)
	var answerLines []string
	inAnswer := false
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		// Stop at obvious UI elements
		lower := strings.ToLower(line)
		if strings.HasPrefix(lower, "new chat") ||
			strings.HasPrefix(lower, "sign in") ||
			strings.HasPrefix(lower, "settings") ||
			strings.HasPrefix(lower, "©") ||
			strings.Contains(lower, "chat.z.ai") ||
			len(line) < 2 {
			if inAnswer {
				break
			}
			continue
		}
		answerLines = append([]string{line}, answerLines...)
		inAnswer = true
	}

	answer := strings.TrimSpace(strings.Join(answerLines, "\n"))
	if answer == "" {
		return "", fmt.Errorf("could not extract answer from page")
	}
	return answer, nil
}

// ───────── Chrome Discovery ─────────

func findChrome() string {
	if p := os.Getenv("CHROME_PATH"); p != "" {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	switch runtime.GOOS {
	case "darwin":
		for _, c := range []string{
			"/Applications/Google Chrome.app/Contents/MacOS/Google Chrome",
			"/Applications/Chromium.app/Contents/MacOS/Chromium",
		} {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	case "linux":
		for _, c := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
			if p, err := exec.LookPath(c); err == nil {
				return p
			}
		}
	case "windows":
		for _, c := range []string{
			`C:\Program Files\Google\Chrome\Application\chrome.exe`,
			`C:\Program Files (x86)\Google\Chrome\Application\chrome.exe`,
		} {
			if _, err := os.Stat(c); err == nil {
				return c
			}
		}
	}
	for _, c := range []string{"google-chrome", "chromium"} {
		if p, err := exec.LookPath(c); err == nil {
			return p
		}
	}

	return ""
}

func randomHex(n int) string {
	b := make([]byte, n)
	for i := range b {
		b[i] = "0123456789abcdef"[time.Now().UnixNano()&0xf]
		time.Sleep(time.Microsecond)
	}
	return string(b)
}
