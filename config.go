package main

import (
	"encoding/hex"
	"log/slog"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/plaid/plaid-go/v43/plaid"
)

const maxPlaidTimeout = 300 * time.Second

type Config struct {
	DBPath            string            `envconfig:"DB_PATH" default:"./data/runway.db"`
	DBCryptKey        []byte            `envconfig:"-"`
	DBCryptKeyHex     string            `envconfig:"DB_CRYPT_KEY"`
	ListenAddress     string            `envconfig:"LISTEN_ADDRESS" default:"localhost:1996"`
	TGBotKey          string            `envconfig:"TG_BOT_KEY"`
	TGChatId          int64             `envconfig:"TG_CHAT_ID"`
	Production        bool              `envconfig:"PRODUCTION" default:"false"`
	PlaidClientID     string            `envconfig:"PLAID_CLIENT_ID"`
	PlaidSecret       string            `envconfig:"PLAID_SECRET"`
	SignalRulesetKey  string            `envconfig:"SIGNAL_RULESET_KEY"`
	PlaidEnv          plaid.Environment `envconfig:"-"`
	PlaidTimeout      time.Duration     `envconfig:"PLAID_TIMEOUT" default:"30s"`
	PlaidEnvName      string            `envconfig:"PLAID_ENV" default:"sandbox"`
	PlaidProducts     string            `envconfig:"PLAID_PRODUCTS" default:"auth,transactions,signal"`
	PlaidCountryCodes string            `envconfig:"PLAID_COUNTRY_CODES" default:"US,CA"`
	PlaidRedirectURI  string            `envconfig:"PLAID_REDIRECT_URI"`
	PlaidWebhookURL   string            `envconfig:"PLAID_WEBHOOK_URL" default:"https://gpws.kawaiide.su/hook/plaid"`
	TokenTTL          time.Duration     `envconfig:"TOKEN_TTL" default:"30m"`
}

var Env Config

func LoadSettings() *Config {
	if err := godotenv.Load(); err != nil {
		slog.Warn(".env file not found, using system environment variables instead")
	}
	if err := envconfig.Process("runway", &Env); err != nil {
		slog.Error("Failed to load environment configuration", "error", err)
		os.Exit(1)
	}
	if Env.DBCryptKeyHex == "" {
		slog.Error("DB_CRYPT_KEY must be set in environment variables")
		os.Exit(1)
	}
	if len(Env.DBCryptKeyHex) != 64 {
		slog.Error("DB_CRYPT_KEY must be 32 bytes long (64 hex characters)")
		os.Exit(1)
	}
	if Env.PlaidTimeout <= 0 {
		slog.Error("PLAID_TIMEOUT must be a positive duration")
		os.Exit(1)
	}
	if Env.PlaidTimeout > maxPlaidTimeout {
		slog.Error("PLAID_TIMEOUT must not exceed 300 seconds")
		os.Exit(1)
	}
	if Env.TokenTTL < time.Minute {
		slog.Error("TOKEN_TTL must be at least one minute")
		os.Exit(1)
	}
	switch Env.PlaidEnvName {
	case "sandbox":
		Env.PlaidEnv = plaid.Sandbox
	case "production":
		Env.PlaidEnv = plaid.Production
	default:
		slog.Error("Invalid PLAID_ENV value", "value", Env.PlaidEnvName)
		os.Exit(1)
	}
	var err error
	Env.DBCryptKey, err = hex.DecodeString(Env.DBCryptKeyHex)
	Env.DBCryptKeyHex = "" // Clear the hex string from memory for security
	if err != nil {
		slog.Error("Failed to decode DB_CRYPT_KEY", "error", err)
		os.Exit(1)
	}
	return &Env
}
