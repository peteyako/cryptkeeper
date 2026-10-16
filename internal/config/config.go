// Package config loads server settings from config.yaml and secrets from the environment.
// Sources (wallets, exchange keys) are not here: they live in data/sources.yaml and are
// managed from the UI.
package config

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	RefreshMinutes  int      `yaml:"refreshMinutes"`
	StaleMaxHours   int      `yaml:"staleMaxHours"`
	ActivityMinutes int      `yaml:"activityMinutes"`
	ShowTokenLogos  bool     `yaml:"showTokenLogos"`
	Bitcoin         Bitcoin  `yaml:"bitcoin"`
	Solana          Solana   `yaml:"solana"`
	Ntfy            Ntfy     `yaml:"ntfy"`
	Activity        Activity `yaml:"activity"`
	// SeedSources are written to data/sources.yaml on first run only.
	SeedSources []map[string]any `yaml:"seedSources"`

	// From the environment, never from the file.
	SecretKey string `yaml:"-"` // CRYPTKEEP_SECRET_KEY
	Password  string `yaml:"-"` // PORTFOLIO_PASSWORD
	User      string `yaml:"-"` // PORTFOLIO_USER
	NtfyToken string `yaml:"-"` // NTFY_TOKEN
	// CoinGeckoKey is the CoinGecko demo API key (COINGECKO_API_KEY); it wins over the one saved in Settings.
	CoinGeckoKey string `yaml:"-"`
}

type Bitcoin struct {
	EsploraURL string `yaml:"esploraUrl"`
	GapLimit   int    `yaml:"gapLimit"`
}

type TokenOverride struct {
	Symbol string `yaml:"symbol"`
	Name   string `yaml:"name"`
}

type Solana struct {
	RPCURL               string                   `yaml:"rpcUrl"`
	IncludeStakeAccounts bool                     `yaml:"includeStakeAccounts"`
	HideUnpriced         bool                     `yaml:"hideUnpriced"`
	MinUSD               float64                  `yaml:"minUsd"`
	HideMints            []string                 `yaml:"hideMints"`
	AllowMints           []string                 `yaml:"allowMints"`
	TokenOverrides       map[string]TokenOverride `yaml:"tokenOverrides"`
	DeFi                 []string                 `yaml:"defi"`
}

type Ntfy struct {
	URL           string `yaml:"url"`
	RedactAmounts bool   `yaml:"redactAmounts"`
}

type Activity struct {
	BackfillDays int `yaml:"backfillDays"`
}

// Default returns the configuration used when no config.yaml exists.
func Default() Config {
	return Config{
		RefreshMinutes:  5,
		StaleMaxHours:   24,
		ActivityMinutes: 15,
		Bitcoin:         Bitcoin{EsploraURL: "https://mempool.space/api", GapLimit: 20},
		Solana: Solana{
			RPCURL:               "https://api.mainnet-beta.solana.com",
			IncludeStakeAccounts: true,
			HideUnpriced:         true,
			MinUSD:               1,
			DeFi:                 []string{"jupiter", "orca", "raydium"},
		},
		Ntfy:     Ntfy{RedactAmounts: true},
		Activity: Activity{BackfillDays: 365},
	}
}

// Load reads path (missing file is fine: defaults are used), applies defaults for zero
// values, then reads secrets from the environment.
func Load(path string) (Config, error) {
	cfg := Default()
	raw, err := os.ReadFile(path)
	switch {
	case err == nil:
		dec := yaml.NewDecoder(bytes.NewReader(raw))
		dec.KnownFields(true)
		if err := dec.Decode(&cfg); err != nil && err.Error() != "EOF" {
			return cfg, fmt.Errorf("%s: %w", path, err)
		}
	case os.IsNotExist(err):
	default:
		return cfg, err
	}
	d := Default()
	if cfg.RefreshMinutes <= 0 {
		cfg.RefreshMinutes = d.RefreshMinutes
	}
	if cfg.StaleMaxHours <= 0 {
		cfg.StaleMaxHours = d.StaleMaxHours
	}
	if cfg.ActivityMinutes <= 0 {
		cfg.ActivityMinutes = d.ActivityMinutes
	}
	if cfg.Bitcoin.EsploraURL == "" {
		cfg.Bitcoin.EsploraURL = d.Bitcoin.EsploraURL
	}
	cfg.Bitcoin.EsploraURL = strings.TrimRight(cfg.Bitcoin.EsploraURL, "/")
	if cfg.Bitcoin.GapLimit <= 0 {
		cfg.Bitcoin.GapLimit = d.Bitcoin.GapLimit
	}
	if cfg.Solana.RPCURL == "" {
		cfg.Solana.RPCURL = d.Solana.RPCURL
	}
	if cfg.Activity.BackfillDays <= 0 {
		cfg.Activity.BackfillDays = d.Activity.BackfillDays
	}
	cfg.SecretKey = os.Getenv("CRYPTKEEP_SECRET_KEY")
	if cfg.SecretKey == "" {
		cfg.SecretKey = os.Getenv("VAULT_SECRET_KEY") // its name before the CryptKeep rename
	}
	cfg.Password = os.Getenv("PORTFOLIO_PASSWORD")
	cfg.User = os.Getenv("PORTFOLIO_USER")
	if cfg.User == "" {
		cfg.User = "admin"
	}
	cfg.NtfyToken = os.Getenv("NTFY_TOKEN")
	cfg.CoinGeckoKey = os.Getenv("COINGECKO_API_KEY")
	return cfg, nil
}

func (c Config) RefreshInterval() time.Duration { return time.Duration(c.RefreshMinutes) * time.Minute }
func (c Config) ActivityInterval() time.Duration {
	return time.Duration(c.ActivityMinutes) * time.Minute
}
func (c Config) StaleMax() time.Duration { return time.Duration(c.StaleMaxHours) * time.Hour }

// EncryptExports reports whether YAML exports are encrypted as a whole: only when the user
// set CRYPTKEEP_SECRET_KEY. The generated data/secret.key lives next to the data, so a
// backup encrypted with it would be lost with the disk it is meant to survive.
func (c Config) EncryptExports() bool { return c.SecretKey != "" }

// LoadDotEnv sets variables from a KEY=VALUE file without overriding the real environment.
// A missing file is not an error.
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' && v[len(v)-1] == '"' || v[0] == '\'' && v[len(v)-1] == '\'') {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}
