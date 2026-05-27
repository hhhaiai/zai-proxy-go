package internal

import "testing"

func TestParseModelName(t *testing.T) {
	tests := []struct {
		input        string
		base         string
		think, search bool
	}{
		{"GLM-4.7", "GLM-4.7", false, false},
		{"GLM-4.7-thinking", "GLM-4.7", true, false},
		{"GLM-4.7-search", "GLM-4.7", false, true},
		{"GLM-4.7-thinking-search", "GLM-4.7", true, true},
		{"GLM-5.1-thinking-search", "GLM-5.1", true, true},
		{"GLM-5-search-thinking", "GLM-5", true, true},
	}
	for _, tt := range tests {
		base, think, search := ParseModelName(tt.input)
		if base != tt.base || think != tt.think || search != tt.search {
			t.Errorf("ParseModelName(%q) = (%q, %v, %v), want (%q, %v, %v)",
				tt.input, base, think, search, tt.base, tt.think, tt.search)
		}
	}
}

func TestGetTargetModel(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"GLM-4.7", "glm-4.7"},
		{"GLM-5", "glm-5"},
		{"GLM-5.1", "GLM-5.1"},
		{"GLM-4.7-thinking", "glm-4.7"},
		{"GLM-5-thinking-search", "glm-5"},
		{"GLM-4.5-V", "glm-4.5v"},
	}
	for _, tt := range tests {
		got := GetTargetModel(tt.input)
		if got != tt.want {
			t.Errorf("GetTargetModel(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsGLM5Model(t *testing.T) {
	if !IsGLM5Model("GLM-5") { t.Error("GLM-5 should be GLM5") }
	if !IsGLM5Model("GLM-5.1-thinking") { t.Error("GLM-5.1-thinking should be GLM5") }
	if IsGLM5Model("GLM-4.7") { t.Error("GLM-4.7 should not be GLM5") }
	if IsGLM5Model("GLM-4.7-thinking-search") { t.Error("GLM-4.7-thinking-search should not be GLM5") }
}
