package internal

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port               string
	CaptchaVerifyParam string // 静态 captcha_verify_param
	CaptchaAPI         string // 动态获取 captcha_verify_param 的 API
}

var Cfg *Config

func LoadConfig() {
	godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	Cfg = &Config{
		Port:               port,
		CaptchaVerifyParam: os.Getenv("ZAI_CAPTCHA_VERIFY_PARAM"),
		CaptchaAPI:         os.Getenv("ZAI_CAPTCHA_API"),
	}
}
