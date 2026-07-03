package runway

import (
	"encoding/hex"
	"log/slog"
	"os"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

type Config struct {
	DBPath            string `envconfig:"DB_PATH" default:"./data/runway.db"`
	DBCryptKey        []byte `envconfig:"-"`
	DBCryptKeyHex     string `envconfig:"DB_CRYPT_KEY"`
	Production        bool   `envconfig:"PRODUCTION" default:"false"`
	PlaidClientID     string `envconfig:"PLAID_CLIENT_ID"`
	PlaidSecret       string `envconfig:"PLAID_SECRET"`
	SignalRulesetKey  string `envconfig:"SIGNAL_RULESET_KEY"`
	PlaidEnv          string `envconfig:"PLAID_ENV" default:"sandbox"`
	PlaidProducts     string `envconfig:"PLAID_PRODUCTS" default:"auth,transactions,signal"`
	PlaidCountryCodes string `envconfig:"PLAID_COUNTRY_CODES" default:"US,CA"`
	PlaidRedirectURI  string `envconfig:"PLAID_REDIRECT_URI"`
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
	var err error
	Env.DBCryptKey, err = hex.DecodeString(Env.DBCryptKeyHex)
	Env.DBCryptKeyHex = "" // Clear the hex string from memory for security
	if err != nil {
		slog.Error("Failed to decode DB_CRYPT_KEY", "error", err)
		os.Exit(1)
	}
	return &Env
}
