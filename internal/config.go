package internal

import (
	"os"

	"github.com/joho/godotenv"
)

type Config struct {
	Port string
}

var Cfg *Config

func LoadConfig() {
	godotenv.Load()

	port := os.Getenv("PORT")
	if port == "" {
		port = "8000"
	}

	Cfg = &Config{
		Port: port,
	}
}
