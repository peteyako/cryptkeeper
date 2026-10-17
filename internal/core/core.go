// Package core holds the types shared by every source, the aggregator and the UI.
// It has no dependencies on other internal packages, so anything can import it.
package core

import (
	"context"
	"strings"
	"time"
)

// Kind is the custody type of a source. It drives the source color and dot shape in the UI.
type Kind string

const (
	KindExchange Kind = "exchange"
	KindHot      Kind = "hot"
	KindHardware Kind = "hardware"
	KindManual   Kind = "manual"
)

// Holding is one asset balance inside one wallet/source.
type Holding struct {
	WalletID string `json:"walletId"`
	// AssetKey identifies the asset across sources: an exchange symbol ("BTC", "USDC")
	// or "spl:<mint>" for SPL tokens that are not in the well-known mint table.
	AssetKey  string  `json:"assetKey"`
	Symbol    string  `json:"symbol"`
	Name      string  `json:"name,omitempty"`
	Mint      string  `json:"mint,omitempty"`
	LogoURL   string  `json:"logoUrl,omitempty"`
	Amount    float64 `json:"amount"`              // negative means debt
	AmountStr string  `json:"amountStr,omitempty"` // exact decimal string when the source provides one
	Staked    bool    `json:"staked,omitempty"`
	Vault     bool    `json:"vault,omitempty"`
	// Verified is set for SPL tokens on Jupiter's verified list (or the known-mint table, or
	// allow-listed by the user). Unverified tokens are hidden by default: scam airdrops often
	// carry a fake Jupiter price that would inflate the total.
	Verified bool `json:"verified,omitempty"`
	// DeFi position tagging; empty for plain balances.
	Protocol     string `json:"protocol,omitempty"`
	Position     string `json:"position,omitempty"`
	PositionKind string `json:"positionKind,omitempty"` // "lp", "perp", "locked"
	Note         string `json:"note,omitempty"`
}

// WalletResult is what one source returns from a fetch.
type WalletResult struct {
	WalletID string    `json:"walletId"`
	Holdings []Holding `json:"holdings"`
	Hidden   []Holding `json:"hidden,omitempty"` // spam/dust/unpriced, shown collapsed
	NFTCount int       `json:"nftCount,omitempty"`
	Warnings []string  `json:"warnings,omitempty"`
	// Meta carries display facts for the wallet detail Connection panel,
	// e.g. "Block height", "Addresses used", "Node", "Derivation".
	Meta  []KV      `json:"meta,omitempty"`
	Err   string    `json:"error,omitempty"`
	Stale bool      `json:"stale,omitempty"` // fetch failed; these are last-good holdings
	AsOf  time.Time `json:"asOf"`
}

// KV is an ordered key/value pair for display.
type KV struct {
	K string `json:"k"`
	V string `json:"v"`
}

// Source is one connected wallet or exchange account.
type Source interface {
	ID() string
	Fetch(ctx context.Context) (WalletResult, error)
}

// Price is a USD price with provenance.
type Price struct {
	USD   float64   `json:"usd"`
	AsOf  time.Time `json:"asOf"`
	Stale bool      `json:"stale,omitempty"` // served from the last-good cache
	// Change24h is the 24h price change in percent when the price source provides it.
	Change24h *float64 `json:"change24h,omitempty"`
}

// Address is a derived or watched on-chain address, for the wallet detail Addresses tab.
type Address struct {
	Address string  `json:"address"`
	Path    string  `json:"path,omitempty"` // e.g. "0/7"
	Change  bool    `json:"change,omitempty"`
	TxCount int     `json:"txCount"`
	Balance float64 `json:"balance"` // in the chain's coin (BTC, SOL)
}

// UTXO is an unspent Bitcoin output.
type UTXO struct {
	TxID      string  `json:"txid"`
	Vout      uint32  `json:"vout"`
	Address   string  `json:"address"`
	Value     float64 `json:"value"` // BTC
	Confirmed bool    `json:"confirmed"`
	Height    int64   `json:"height,omitempty"`
}

// AddressLister is implemented by sources that can list their addresses (Bitcoin, Solana).
type AddressLister interface {
	Addresses(ctx context.Context) ([]Address, error)
}

// UTXOLister is implemented by Bitcoin sources.
type UTXOLister interface {
	UTXOs(ctx context.Context) ([]UTXO, error)
}

// ReceiveAddresser is implemented by HD Bitcoin sources: the next unused receive address.
type ReceiveAddresser interface {
	NextReceiveAddress(ctx context.Context) (string, error)
}

// SPLKey returns the asset key for an unmapped SPL mint.
func SPLKey(mint string) string { return "spl:" + mint }

// IsSPLKey reports whether key is an "spl:<mint>" key, returning the mint.
func IsSPLKey(key string) (string, bool) {
	if m, ok := strings.CutPrefix(key, "spl:"); ok {
		return m, true
	}
	return "", false
}

// Stablecoins pegged to 1 USD when no price is available.
var Pegged = map[string]bool{"USD": true, "USDC": true, "USDT": true}

// stableSymbols are the USD stablecoins (and cash) by ticker, for IsStable.
var stableSymbols = map[string]bool{
	"USD": true, "USDC": true, "USDT": true, "DAI": true, "PYUSD": true, "USDS": true, "FDUSD": true,
	"TUSD": true, "USDE": true, "USDG": true, "USD1": true, "USDP": true, "GUSD": true, "BUSD": true,
}

// IsStable reports whether a holding is cash or a USD stablecoin, by asset key or ticker.
func IsStable(assetKey, symbol string) bool {
	return Pegged[assetKey] || stableSymbols[strings.ToUpper(symbol)]
}
