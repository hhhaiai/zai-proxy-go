# ── Build stage ──
FROM golang:1.26-alpine AS builder

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -ldflags="-s -w" -o zai-proxy .

# ── Runtime stage ──
FROM alpine:3.20

# Install minimal Chromium for headless browser (anonymous mode captcha solving)
RUN apk add --no-cache \
    chromium \
    nss \
    freetype \
    harfbuzz \
    ttf-freefont \
    font-noto-emoji \
    && rm -rf /var/cache/apk/*

# Set Chromium path
ENV CHROME_PATH=/usr/bin/chromium-browser
ENV CHROME_BIN=/usr/bin/chromium-browser

WORKDIR /app

COPY --from=builder /app/zai-proxy .

EXPOSE 8000

# Run with minimal resource limits
CMD ["./zai-proxy"]
