package main

import (
	"encoding/hex"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
	"github.com/plaid/plaid-go/v43/plaid"
)

const maxPlaidTimeout = 300 * time.Second

type Config struct {
	DBPath        string `envconfig:"DB_PATH" default:"./data/runway.db"`
	DBCryptKey    []byte `envconfig:"-"`
	DBCryptKeyHex string `envconfig:"DB_CRYPT_KEY"`
	ListenAddress string `envconfig:"LISTEN_ADDRESS" default:"localhost:1996"`
	TGBotKey      string `envconfig:"TG_BOT_KEY"`
	// BaseURL is the public origin this service is reachable at; the /link
	// URL and the default Plaid webhook URL are derived from it.
	BaseURL           string            `envconfig:"BASE_URL" default:"https://gpws.kawaiide.su"`
	PlaidClientID     string            `envconfig:"PLAID_CLIENT_ID"`
	PlaidSecret       string            `envconfig:"PLAID_SECRET"`
	PlaidEnv          plaid.Environment `envconfig:"-"`
	PlaidTimeout      time.Duration     `envconfig:"PLAID_TIMEOUT" default:"30s"`
	PlaidEnvName      string            `envconfig:"PLAID_ENV" default:"sandbox"`
	PlaidProducts     string            `envconfig:"PLAID_PRODUCTS" default:"transactions"`
	PlaidCountryCodes string            `envconfig:"PLAID_COUNTRY_CODES" default:"US"`
	PlaidWebhookURL   string            `envconfig:"PLAID_WEBHOOK_URL"`
	// PlaidHistoryDays is how much transaction history Plaid pulls when an
	// item is first linked (Plaid caps this at 730; its default is only 90).
	PlaidHistoryDays int32         `envconfig:"PLAID_HISTORY_DAYS" default:"730"`
	TokenTTL         time.Duration `envconfig:"TOKEN_TTL" default:"30m"`

	// Parsed from PlaidProducts / PlaidCountryCodes; used when creating link tokens.
	PlaidProductList     []plaid.Products    `envconfig:"-"`
	PlaidCountryCodeList []plaid.CountryCode `envconfig:"-"`
}

func LoadSettings() *Config {
	var cfg Config
	if err := godotenv.Load(); err != nil {
		slog.Warn(".env file not found, using system environment variables instead")
	}
	if err := envconfig.Process("runway", &cfg); err != nil {
		slog.Error("Failed to load environment configuration", "error", err)
		os.Exit(1)
	}
	if cfg.DBCryptKeyHex == "" {
		slog.Error("DB_CRYPT_KEY must be set in environment variables")
		os.Exit(1)
	}
	if len(cfg.DBCryptKeyHex) != 64 {
		slog.Error("DB_CRYPT_KEY must be 32 bytes long (64 hex characters)")
		os.Exit(1)
	}
	if cfg.PlaidTimeout <= 0 {
		slog.Error("PLAID_TIMEOUT must be a positive duration")
		os.Exit(1)
	}
	if cfg.PlaidTimeout > maxPlaidTimeout {
		slog.Error("PLAID_TIMEOUT must not exceed 300 seconds")
		os.Exit(1)
	}
	if cfg.TokenTTL < time.Minute {
		slog.Error("TOKEN_TTL must be at least one minute")
		os.Exit(1)
	}
	if cfg.PlaidHistoryDays < 1 || cfg.PlaidHistoryDays > 730 {
		slog.Error("PLAID_HISTORY_DAYS must be between 1 and 730")
		os.Exit(1)
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	if u, err := url.Parse(cfg.BaseURL); err != nil || u.Scheme == "" || u.Host == "" {
		slog.Error("BASE_URL must be an absolute URL", "value", cfg.BaseURL)
		os.Exit(1)
	}
	if cfg.PlaidWebhookURL == "" {
		cfg.PlaidWebhookURL = cfg.BaseURL + "/hook/plaid"
	}
	for _, p := range strings.Split(cfg.PlaidProducts, ",") {
		product, err := plaid.NewProductsFromValue(strings.ToLower(strings.TrimSpace(p)))
		if err != nil {
			slog.Error("Invalid PLAID_PRODUCTS value", "value", p, "error", err)
			os.Exit(1)
		}
		cfg.PlaidProductList = append(cfg.PlaidProductList, *product)
	}
	for _, c := range strings.Split(cfg.PlaidCountryCodes, ",") {
		country, err := plaid.NewCountryCodeFromValue(strings.ToUpper(strings.TrimSpace(c)))
		if err != nil {
			slog.Error("Invalid PLAID_COUNTRY_CODES value", "value", c, "error", err)
			os.Exit(1)
		}
		cfg.PlaidCountryCodeList = append(cfg.PlaidCountryCodeList, *country)
	}
	switch cfg.PlaidEnvName {
	case "sandbox":
		cfg.PlaidEnv = plaid.Sandbox
	case "production":
		cfg.PlaidEnv = plaid.Production
	default:
		slog.Error("Invalid PLAID_ENV value", "value", cfg.PlaidEnvName)
		os.Exit(1)
	}
	var err error
	cfg.DBCryptKey, err = hex.DecodeString(cfg.DBCryptKeyHex)
	cfg.DBCryptKeyHex = "" // Clear the hex string from memory for security
	if err != nil {
		slog.Error("Failed to decode DB_CRYPT_KEY", "error", err)
		os.Exit(1)
	}
	return &cfg
}
