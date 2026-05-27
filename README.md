# zai-proxy

zai-proxy 是一个基于 Go 语言的代理服务，将 z.ai 网页聊天转换为 OpenAI API 兼容格式。用户使用自己的 z.ai token 进行调用。

## 功能特性

- OpenAI API 兼容格式
- Anthropic Messages API 兼容格式（支持 Claude Code）
- 支持流式和非流式响应
- 支持多种 GLM 模型（GLM-4.5/4.6/4.7/5/5.1）
- 支持思考模式 (thinking)
- 支持联网搜索模式 (search)
- 支持多模态图片输入
- 支持匿名 Token（免登录）
- Token 管理器（轮询、动态获取、健康检查）
- **自动生成签名**
- **自动更新签名版本号**

## 快速开始

### 安装运行

```bash
# 克隆项目
git clone https://github.com/kao0312/zai-proxy.git
cd zai-proxy

# 安装依赖
go mod download

# 运行服务
go run main.go
```

### Docker 一键部署

```bash
docker run -d -p 8000:8000 ghcr.io/kao0312/zai-proxy:latest
```

自定义端口和日志级别：

```bash
docker run -d -p 8080:8000 -e LOG_LEVEL=debug ghcr.io/kao0312/zai-proxy:latest
```

## 环境变量

### 基础配置

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| PORT | 监听端口 | 8000 |
| LOG_LEVEL | 日志级别 | info |

### Token 管理配置

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| ZAI_STATIC_TOKENS | 静态 Token 列表（逗号分隔） | 空 |
| ZAI_ENABLE_ANONYMOUS | 是否启用匿名 Token | true |
| ZAI_STRATEGY | 轮询策略 | round_robin |
| ZAI_DYNAMIC_API | 动态获取 Token 的 API 地址 | 空 |
| ZAI_DYNAMIC_API_TOKEN | 动态 API 认证 Token | 空 |
| ZAI_DYNAMIC_INTERVAL | 动态获取间隔（分钟） | 30 |
| ZAI_ENABLE_HEALTH_CHECK | 是否启用健康检查 | true |
| ZAI_HEALTH_CHECK_INTERVAL | 健康检查间隔（秒） | 60 |

### 轮询策略说明

- `round_robin` - 顺序轮询（默认）
- `random` - 随机选择
- `least_used` - 使用次数最少优先

## 获取 z.ai Token

### 方式一：使用匿名 Token（免登录）

直接使用 `free` 作为 API key，服务会自动获取一个匿名 token：

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer free" \
  -H "Content-Type: application/json" \
  -d '{"model": "GLM-4.7", "messages": [{"role": "user", "content": "hello"}]}'
```

> 注意：匿名模式只有 `GLM-4.7` 可用，GLM-5/5.1 需要登录后使用。

### 方式二：使用个人 Token

1. 登录 https://chat.z.ai
2. 打开浏览器开发者工具 (F12)
3. 切换到 Application/Storage 标签
4. 在 Cookies 中找到 `token` 字段
5. 复制其值作为 API 调用的 Authorization

### 方式三：使用 Token 管理器

配置多个 Token 进行轮询：

```bash
# .env 文件
ZAI_STATIC_TOKENS=token1,token2,token3
ZAI_STRATEGY=round_robin
ZAI_ENABLE_ANONYMOUS=true
```

使用 `managed` 关键字触发 Token 管理器：

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer managed" \
  -H "Content-Type: application/json" \
  -d '{"model": "GLM-5.1", "messages": [{"role": "user", "content": "hello"}]}'
```

## 支持的模型

| 模型名称 | 上游模型 | 匿名可用 | 登录可用 |
|----------|----------|----------|----------|
| GLM-4.5 | 0727-360B-API | ✓ | ✓ |
| GLM-4.6 | GLM-4-6-API-V1 | ✓ | ✓ |
| GLM-4.7 | glm-4.7 | ✓ | ✓ |
| GLM-5 | glm-5 | ✗ | ✓ |
| GLM-5.1 | GLM-5.1 | ✗ | ✓ |
| GLM-4.5-V | glm-4.5v | ✓ | ✓ |
| GLM-4.6-V | glm-4.6v | ✓ | ✓ |
| GLM-4.5-Air | 0727-106B-API | ✓ | ✓ |

### 模型标签

模型名称支持以下后缀标签（可组合使用）：

- `-thinking`: 启用思考模式，响应会包含 `reasoning_content` 字段
- `-search`: 启用联网搜索模式

示例：

- `GLM-5.1-thinking`
- `GLM-5.1-search`
- `GLM-5.1-thinking-search`

## 使用示例

### curl 测试

```bash
# 基础请求
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer YOUR_ZAI_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-5.1",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'

# 使用思考模式
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer YOUR_ZAI_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-5.1-thinking",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'

# 使用联网搜索
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer YOUR_ZAI_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-5.1-search",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

### Claude Code / Anthropic Messages API

本项目支持 Anthropic Messages API 格式 (`/v1/messages`)，可直接用于 Claude Code：

**Claude Code 配置：**

```bash
# 设置 API endpoint
export ANTHROPIC_BASE_URL=http://localhost:8000

# 设置 API key（使用 z.ai token 或 "free" 使用匿名 token）
export ANTHROPIC_API_KEY=free

# 启动 Claude Code
claude
```

**curl 测试 Anthropic 格式：**

```bash
curl http://localhost:8000/v1/messages \
  -H "x-api-key: free" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model": "claude-opus-4-6",
    "max_tokens": 1024,
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

> 注意：无论传入什么模型名称（如 `claude-opus-4-6`），内部实际调用的是 `GLM-5.1-thinking-search`。

### 多模态请求

```json
{
  "model": "GLM-4.6-V",
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "描述这张图片"},
        {"type": "image_url", "image_url": {"url": "https://example.com/image.jpg"}}
      ]
    }
  ]
}
```

### 支持的图片格式

- HTTP/HTTPS URL
- Base64 编码 (data:image/jpeg;base64,...)

## Token 管理 API

### 查看 Token 状态

```bash
curl http://localhost:8000/v1/tokens
```

### 添加 Token

```bash
curl -X POST http://localhost:8000/v1/tokens/add \
  -H "Content-Type: application/json" \
  -d '{"token": "your-token", "source": "static"}'
```

### 查看统计信息

```bash
curl http://localhost:8000/v1/tokens/stats
```

## 配置示例

### 基础多 Token 轮询

```bash
ZAI_STATIC_TOKENS=token1,token2,token3,token4,token5
ZAI_STRATEGY=round_robin
```

### 混合模式（静态+动态+匿名）

```bash
ZAI_STATIC_TOKENS=personal-token-1,personal-token-2
ZAI_DYNAMIC_API=https://your-api.com/get-token
ZAI_DYNAMIC_API_TOKEN=your-secret
ZAI_ENABLE_ANONYMOUS=true
ZAI_STRATEGY=least_used
```

### 纯匿名模式（保持原版功能）

```bash
ZAI_ENABLE_ANONYMOUS=true
ZAI_STATIC_TOKENS=
```

## 架构说明

```
zai-proxy/
├── main.go              # 入口文件
├── internal/
│   ├── anthropic.go     # Anthropic API 兼容层
│   ├── chat.go          # OpenAI API 处理
│   ├── models.go        # 模型定义和映射
│   ├── token_manager.go # Token 管理器
│   ├── token_handlers.go # Token API 处理
│   ├── anonymous.go     # 匿名 Token 获取
│   ├── config.go        # 配置加载
│   ├── jwt.go           # JWT 解析
│   ├── logger.go        # 日志系统
│   ├── signature.go     # 签名生成
│   ├── upload.go        # 图片上传
│   └── version.go       # 版本管理
├── go.mod
├── go.sum
├── Dockerfile
└── .env.example
```

## 许可证

MIT License
