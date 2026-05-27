# zai-proxy

zai-proxy 是一个基于 Go 语言的代理服务，将 z.ai 网页聊天转换为 OpenAI API 兼容格式。

## 功能特性

- **OpenAI API 兼容格式** (`/v1/chat/completions`)
- **Anthropic Messages API 兼容格式** (`/v1/messages`) - 支持 Claude Code
- **OpenAI Responses API** (`/v1/responses`)
- 支持流式和非流式响应
- 支持多种 GLM 模型（GLM-4.5/4.6/4.7/5/5.1）
- 支持思考模式 (thinking) 和联网搜索模式 (search)
- 支持多模态图片输入
- **匿名模式** - 使用 `free` 作为 API key 自动获取匿名访问
- **自动验证码解决** - 使用 chromedp 自动处理 Aliyun 滑动验证码
- **Token 管理器** - 支持轮询、动态获取、健康检查
- **自动生成签名** - 自动更新签名版本号

## 快速开始

### Docker 一键部署（推荐）

```bash
docker run -d -p 8000:8000 ghcr.io/hhhaiai/zai-proxy-go:latest
```

### Docker Compose

```bash
git clone https://github.com/hhhaiai/zai-proxy-go.git
cd zai-proxy-go
docker-compose up -d
```

### 本地运行

```bash
git clone https://github.com/hhhaiai/zai-proxy-go.git
cd zai-proxy-go
go mod download
go run main.go
```

## 使用方法

### 1. 匿名模式（免登录）

使用 `free` 作为 API key，服务会自动获取匿名 token 并处理验证码：

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer free" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-4.7",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

### 2. Claude Code 集成

```bash
export ANTHROPIC_BASE_URL=http://localhost:8000
export ANTHROPIC_API_KEY=free
claude
```

### 3. 使用个人 Token

1. 登录 https://chat.z.ai
2. 打开浏览器开发者工具 (F12)
3. 切换到 Application/Storage 标签
4. 在 localStorage 中找到 `token` 字段
5. 复制其值作为 API 调用的 Authorization

```bash
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer YOUR_ZAI_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-5.1",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

### 4. Token 管理器

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
  -d '{
    "model": "GLM-5.1",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

## 支持的模型

| 模型名称 | 匿名可用 | 登录可用 |
|----------|----------|----------|
| GLM-4.5 | ✓ | ✓ |
| GLM-4.6 | ✓ | ✓ |
| GLM-4.7 | ✓ | ✓ |
| GLM-5 | ✗ | ✓ |
| GLM-5.1 | ✗ | ✓ |
| GLM-4.5-V | ✓ | ✓ |
| GLM-4.6-V | ✓ | ✓ |
| GLM-4.5-Air | ✓ | ✓ |

支持后缀标签：`-thinking`（思考模式）、`-search`（联网搜索）

## 环境变量

| 变量名 | 说明 | 默认值 |
|--------|------|--------|
| PORT | 监听端口 | 8000 |
| LOG_LEVEL | 日志级别 | info |
| BROWSER_POOL_SIZE | 浏览器会话池大小 | 2 |
| ZAI_STATIC_TOKENS | 静态 Token 列表（逗号分隔） | 空 |
| ZAI_ENABLE_ANONYMOUS | 是否启用匿名 Token | true |
| ZAI_STRATEGY | 轮询策略 | round_robin |

## Token 管理 API

```bash
# 查看 Token 状态
curl http://localhost:8000/v1/tokens

# 添加 Token
curl -X POST http://localhost:8000/v1/tokens/add \
  -H "Content-Type: application/json" \
  -d '{"token": "your-token", "source": "static"}'

# 查看统计信息
curl http://localhost:8000/v1/tokens/stats

# 浏览器池状态
curl http://localhost:8000/browser/status
```

## 使用示例

### curl 测试

```bash
# 基础请求
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer free" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-4.7",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'

# 使用思考模式
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer free" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-4.7-thinking",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'

# 使用联网搜索
curl http://localhost:8000/v1/chat/completions \
  -H "Authorization: Bearer free" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "GLM-4.7-search",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

### Anthropic Messages API

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

## 架构说明

```
zai-proxy/
├── main.go                    # 入口文件
├── internal/
│   ├── anthropic.go          # Anthropic API 兼容层
│   ├── chat.go               # OpenAI API 处理
│   ├── models.go             # 模型定义和映射
│   ├── token_manager.go      # Token 管理器
│   ├── anonymous.go          # 匿名 Token 获取
│   ├── browserproxy/
│   │   └── pool.go           # chromedp 浏览器池（自动验证码）
│   ├── config.go             # 配置加载
│   ├── jwt.go                # JWT 解析
│   ├── signature.go          # 签名生成
│   └── upload.go             # 图片上传
├── Dockerfile
└── docker-compose.yml
```

## 许可证

MIT License
