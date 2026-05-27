package internal

import (
	"encoding/json"
	"net/http"
	"strings"
)

// TokenListResponse Token列表响应
type TokenListResponse struct {
	Success bool        `json:"success"`
	Data    []TokenInfo `json:"data"`
	Stats   interface{} `json:"stats,omitempty"`
}

// TokenAddRequest 添加Token请求
type TokenAddRequest struct {
	Token  string `json:"token"`
	Source string `json:"source"` // "static", "dynamic"
}

// TokenAddResponse 添加Token响应
type TokenAddResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
}

// TokenStatsResponse 统计信息响应
type TokenStatsResponse struct {
	Success bool                   `json:"success"`
	Stats   map[string]interface{} `json:"stats"`
}

// HandleTokenList 查看所有Token状态
func HandleTokenList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	manager := GetTokenManager()
	tokens := manager.GetTokenList()
	stats := manager.GetStats()

	response := TokenListResponse{
		Success: true,
		Data:    tokens,
		Stats:   stats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleTokenAdd 添加新Token
func HandleTokenAdd(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req TokenAddRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Invalid JSON", http.StatusBadRequest)
		return
	}

	// 验证token
	if strings.TrimSpace(req.Token) == "" {
		http.Error(w, "Token is required", http.StatusBadRequest)
		return
	}

	// 解析source
	var source TokenSource
	switch req.Source {
	case "dynamic":
		source = SourceDynamic
	case "static":
		source = SourceStatic
	default:
		source = SourceStatic
	}

	// 添加到管理器
	manager := GetTokenManager()
	manager.AddToken(req.Token, source)

	response := TokenAddResponse{
		Success: true,
		Message: "Token added successfully",
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

// HandleTokenStats 获取统计信息
func HandleTokenStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	manager := GetTokenManager()
	stats := manager.GetStats()

	response := TokenStatsResponse{
		Success: true,
		Stats:   stats,
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}
