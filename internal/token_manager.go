package internal

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// TokenSource Token获取源的类型
type TokenSource string

const (
	SourceAnonymous TokenSource = "anonymous" // 匿名获取
	SourceStatic    TokenSource = "static"    // 静态配置
	SourceDynamic   TokenSource = "dynamic"   // 动态API获取
)

// TokenInfo Token信息结构
type TokenInfo struct {
	Token      string      `json:"token"`
	Source     TokenSource `json:"source"`
	LastUsed   time.Time   `json:"last_used"`
	UsageCount int64       `json:"usage_count"`
	LastError  string      `json:"last_error"`
	IsValid    bool        `json:"is_valid"`
	Expiry     time.Time   `json:"expiry"`
}

// TokenManager Token管理器
type TokenManager struct {
	tokens       []*TokenInfo
	currentIndex int
	lock         sync.RWMutex
	config       *TokenConfig
}

// TokenConfig Token配置
type TokenConfig struct {
	// 静态Token列表（用户提供的个人token）
	StaticTokens []string `json:"static_tokens"`

	// 动态获取配置
	DynamicAPI      string `json:"dynamic_api"`      // 获取token的API地址
	DynamicAPIToken string `json:"dynamic_api_token"` // API认证token
	DynamicInterval int    `json:"dynamic_interval"` // 获取间隔（分钟）

	// 匿名Token配置
	EnableAnonymous bool `json:"enable_anonymous"`

	// 轮询策略
	Strategy string `json:"strategy"` // "round_robin", "random", "least_used"

	// 健康检查
	EnableHealthCheck bool `json:"enable_health_check"`
	HealthCheckInterval int `json:"health_check_interval"` // 秒
}

// TokenResponse 动态API响应格式
type TokenResponse struct {
	Token  string `json:"token"`
	Expiry int64  `json:"expiry"` // 过期时间戳（秒）
}

var (
	tokenManager *TokenManager
	once         sync.Once
)

// GetTokenManager 获取单例Token管理器
func GetTokenManager() *TokenManager {
	once.Do(func() {
		tokenManager = &TokenManager{
			tokens: make([]*TokenInfo, 0),
			config: loadTokenConfig(),
		}
		tokenManager.initialize()
	})
	return tokenManager
}

// loadTokenConfig 从环境变量加载配置
func loadTokenConfig() *TokenConfig {
	config := &TokenConfig{
		Strategy:          "round_robin",
		EnableHealthCheck: true,
		HealthCheckInterval: 60,
		DynamicInterval:   30,
	}

	// 从环境变量读取静态tokens
	if staticTokens := getEnv("ZAI_STATIC_TOKENS", ""); staticTokens != "" {
		config.StaticTokens = strings.Split(staticTokens, ",")
	}

	// 动态API配置
	config.DynamicAPI = getEnv("ZAI_DYNAMIC_API", "")
	config.DynamicAPIToken = getEnv("ZAI_DYNAMIC_API_TOKEN", "")

	// 匿名Token开关
	config.EnableAnonymous = getEnv("ZAI_ENABLE_ANONYMOUS", "true") == "true"

	// 轮询策略
	config.Strategy = getEnv("ZAI_STRATEGY", "round_robin")

	// 健康检查
	config.EnableHealthCheck = getEnv("ZAI_ENABLE_HEALTH_CHECK", "true") == "true"
	config.HealthCheckInterval = getEnvInt("ZAI_HEALTH_CHECK_INTERVAL", 60)

	return config
}

// initialize 初始化Token管理器
func (tm *TokenManager) initialize() {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	// 1. 添加静态Tokens
	for _, token := range tm.config.StaticTokens {
		if token != "" {
			tm.tokens = append(tm.tokens, &TokenInfo{
				Token:     token,
				Source:    SourceStatic,
				IsValid:   true,
				LastUsed:  time.Time{},
				Expiry:    time.Now().Add(24 * time.Hour), // 假设24小时过期
			})
		}
	}

	// 2. 如果启用匿名，添加匿名源
	if tm.config.EnableAnonymous {
		tm.tokens = append(tm.tokens, &TokenInfo{
			Token:   "ANONYMOUS_SOURCE",
			Source:  SourceAnonymous,
			IsValid: true,
		})
	}

	// 3. 如果配置了动态API，启动定时获取器
	if tm.config.DynamicAPI != "" {
		go tm.startDynamicTokenFetcher()
	}

	// 4. 如果启用健康检查，启动健康检查器
	if tm.config.EnableHealthCheck && len(tm.tokens) > 0 {
		go tm.startHealthChecker()
	}

	LogInfo("TokenManager initialized with %d tokens", len(tm.tokens))
}

// GetNextToken 获取下一个可用Token
func (tm *TokenManager) GetNextToken() (string, error) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	if len(tm.tokens) == 0 {
		return "", fmt.Errorf("no available tokens")
	}

	var tokenInfo *TokenInfo

	switch tm.config.Strategy {
	case "random":
		tokenInfo = tm.getRandomToken()
	case "least_used":
		tokenInfo = tm.getLeastUsedToken()
	default: // round_robin
		tokenInfo = tm.getRoundRobinToken()
	}

	if tokenInfo == nil {
		return "", fmt.Errorf("no valid token found")
	}

	// 更新使用统计
	tm.updateUsage(tokenInfo)

	// 如果是匿名源，动态获取实际token
	if tokenInfo.Source == SourceAnonymous {
		return GetAnonymousToken()
	}

	return tokenInfo.Token, nil
}

// getRoundRobinToken 轮询策略
func (tm *TokenManager) getRoundRobinToken() *TokenInfo {
	if len(tm.tokens) == 0 {
		return nil
	}

	// 找到下一个有效的token
	startIndex := tm.currentIndex
	for i := 0; i < len(tm.tokens); i++ {
		idx := (startIndex + i) % len(tm.tokens)
		if tm.tokens[idx].IsValid {
			tm.currentIndex = (idx + 1) % len(tm.tokens)
			return tm.tokens[idx]
		}
	}

	return nil
}

// getRandomToken 随机策略
func (tm *TokenManager) getRandomToken() *TokenInfo {
	validTokens := []*TokenInfo{}
	for _, token := range tm.tokens {
		if token.IsValid {
			validTokens = append(validTokens, token)
		}
	}

	if len(validTokens) == 0 {
		return nil
	}

	// 简单随机选择
	return validTokens[time.Now().UnixNano()%int64(len(validTokens))]
}

// getLeastUsedToken 最少使用策略
func (tm *TokenManager) getLeastUsedToken() *TokenInfo {
	var leastUsed *TokenInfo
	minCount := int64(-1)

	for _, token := range tm.tokens {
		if !token.IsValid {
			continue
		}

		if minCount == -1 || token.UsageCount < minCount {
			minCount = token.UsageCount
			leastUsed = token
		}
	}

	return leastUsed
}

// updateUsage 更新使用统计
func (tm *TokenManager) updateUsage(tokenInfo *TokenInfo) {
	tokenInfo.LastUsed = time.Now()
	tokenInfo.UsageCount++
}

// AddToken 动态添加Token
func (tm *TokenManager) AddToken(token string, source TokenSource) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	// 检查是否已存在
	for _, t := range tm.tokens {
		if t.Token == token {
			return
		}
	}

	tm.tokens = append(tm.tokens, &TokenInfo{
		Token:     token,
		Source:    source,
		IsValid:   true,
		LastUsed:  time.Time{},
		UsageCount: 0,
		Expiry:    time.Now().Add(24 * time.Hour),
	})

	LogInfo("Added new token from %s, total tokens: %d", source, len(tm.tokens))
}

// RemoveToken 移除Token
func (tm *TokenManager) RemoveToken(token string) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	for i, t := range tm.tokens {
		if t.Token == token {
			tm.tokens = append(tm.tokens[:i], tm.tokens[i+1:]...)
			LogInfo("Removed token: %s", token[:10]+"...")
			return
		}
	}
}

// InvalidateToken 标记Token无效
func (tm *TokenManager) InvalidateToken(token string, reason string) {
	tm.lock.Lock()
	defer tm.lock.Unlock()

	for _, t := range tm.tokens {
		if t.Token == token {
			t.IsValid = false
			t.LastError = reason
			LogWarn("Invalidated token: %s, reason: %s", token[:10]+"...", reason)
			return
		}
	}
}

// startDynamicTokenFetcher 动态获取Token（后台任务）
func (tm *TokenManager) startDynamicTokenFetcher() {
	interval := time.Duration(tm.config.DynamicInterval) * time.Minute
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	LogInfo("Dynamic token fetcher started, interval: %d minutes", tm.config.DynamicInterval)

	for range ticker.C {
		token, err := tm.fetchDynamicToken()
		if err != nil {
			LogError("Failed to fetch dynamic token: %v", err)
			continue
		}

		if token != "" {
			tm.AddToken(token, SourceDynamic)
		}
	}
}

// fetchDynamicToken 从动态API获取Token
func (tm *TokenManager) fetchDynamicToken() (string, error) {
	if tm.config.DynamicAPI == "" {
		return "", fmt.Errorf("dynamic API not configured")
	}

	req, err := http.NewRequest("GET", tm.config.DynamicAPI, nil)
	if err != nil {
		return "", err
	}

	// 添加认证（如果需要）
	if tm.config.DynamicAPIToken != "" {
		req.Header.Set("Authorization", "Bearer "+tm.config.DynamicAPIToken)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	var tokenResp TokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
		return "", err
	}

	return tokenResp.Token, nil
}

// startHealthChecker 健康检查器（后台任务）
func (tm *TokenManager) startHealthChecker() {
	interval := time.Duration(tm.config.HealthCheckInterval) * time.Second
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	LogInfo("Health checker started, interval: %d seconds", tm.config.HealthCheckInterval)

	for range ticker.C {
		tm.checkAllTokens()
	}
}

// checkAllTokens 检查所有Token的有效性
func (tm *TokenManager) checkAllTokens() {
	tm.lock.RLock()
	tokens := make([]*TokenInfo, len(tm.tokens))
	copy(tokens, tm.tokens)
	tm.lock.RUnlock()

	for _, tokenInfo := range tokens {
		if tokenInfo.Source == SourceAnonymous {
			continue // 匿名源不需要检查
		}

		// 检查是否过期
		if time.Now().After(tokenInfo.Expiry) {
			tm.InvalidateToken(tokenInfo.Token, "expired")
			continue
		}

		// 检查是否长时间未使用（超过24小时）
		if !tokenInfo.LastUsed.IsZero() &&
			time.Since(tokenInfo.LastUsed) > 24*time.Hour {
			// 可以选择移除或标记
			LogDebug("Token %s not used for 24h", tokenInfo.Token[:10]+"...")
		}
	}
}

// GetStats 获取Token统计信息
func (tm *TokenManager) GetStats() map[string]interface{} {
	tm.lock.RLock()
	defer tm.lock.RUnlock()

	stats := map[string]interface{}{
		"total_tokens":    len(tm.tokens),
		"valid_tokens":    0,
		"invalid_tokens":  0,
		"total_usage":     int64(0),
		"last_updated":    time.Now(),
		"strategy":        tm.config.Strategy,
	}

	for _, token := range tm.tokens {
		if token.IsValid {
			stats["valid_tokens"] = stats["valid_tokens"].(int) + 1
		} else {
			stats["invalid_tokens"] = stats["invalid_tokens"].(int) + 1
		}
		stats["total_usage"] = stats["total_usage"].(int64) + token.UsageCount
	}

	return stats
}

// GetTokenList 获取Token列表（用于管理）
func (tm *TokenManager) GetTokenList() []TokenInfo {
	tm.lock.RLock()
	defer tm.lock.RUnlock()

	result := make([]TokenInfo, len(tm.tokens))
	for i, token := range tm.tokens {
		// 隐藏完整token，只显示前10位
		maskedToken := token.Token
		if len(maskedToken) > 10 {
			maskedToken = maskedToken[:10] + "..."
		}

		result[i] = TokenInfo{
			Token:      maskedToken,
			Source:     token.Source,
			LastUsed:   token.LastUsed,
			UsageCount: token.UsageCount,
			LastError:  token.LastError,
			IsValid:    token.IsValid,
			Expiry:     token.Expiry,
		}
	}
	return result
}

// 辅助函数：获取环境变量
func getEnv(key, defaultValue string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return defaultValue
	}
	return value
}

// 辅助函数：获取整型环境变量
func getEnvInt(key string, defaultValue int) int {
	value := getEnv(key, "")
	if value == "" {
		return defaultValue
	}
	var result int
	fmt.Sscanf(value, "%d", &result)
	return result
}
