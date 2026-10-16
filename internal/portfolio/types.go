// Package portfolio aggregates source results into a priced Snapshot, refreshes it in the
// background, publishes updates to SSE subscribers and records history.
package portfolio

import (
	"time"

	"crypto-portfolio/internal/core"
)

// Status of a wallet in the last refresh.
type Status string

const (
	StatusSynced  Status = "synced"
	StatusStale   Status = "stale"
	StatusError   Status = "error"
	StatusPending Status = "pending" // never fetched yet
)

// Snapshot is the fully priced state of everything (or of one portfolio, after filtering).
type Snapshot struct {
	UpdatedAt time.Time             `json:"updatedAt"`
	TotalUSD  float64               `json:"totalUsd"`
	HasStale  bool                  `json:"hasStale"` // total includes stale prices or balances
	Assets    []AssetRow            `json:"assets"`   // merged across wallets, sorted by value desc
	Wallets   []WalletView          `json:"wallets"`
	Prices    map[string]core.Price `json:"prices"`
}

// AssetRow is one asset merged across all wallets in the snapshot.
type AssetRow struct {
	AssetKey  string      `json:"assetKey"`
	Symbol    string      `json:"symbol"`
	Name      string      `json:"name,omitempty"`
	Mint      string      `json:"mint,omitempty"`
	LogoURL   string      `json:"logoUrl,omitempty"`
	Amount    float64     `json:"amount"`
	Price     *core.Price `json:"price,omitempty"`
	ValueUSD  float64     `json:"valueUsd"`
	Priced    bool        `json:"priced"`
	Share     float64     `json:"share"`  // 0..1 of snapshot total
	HeldIn    []string    `json:"heldIn"` // wallet labels
	StakedAmt float64     `json:"stakedAmount,omitempty"`
	// Change7d is the 7-day price change in percent, from recorded history (nil until
	// history covers a week).
	Change7d *float64 `json:"change7d,omitempty"`
}

// WalletView is one source with its priced holdings.
type WalletView struct {
	ID         string       `json:"id"`
	Label      string       `json:"label"`
	Type       string       `json:"type"` // coinbase, kraken, bitcoin, solana, manual
	Kind       core.Kind    `json:"kind"`
	Rows       []HoldingRow `json:"rows"`
	HiddenRows []HoldingRow `json:"hiddenRows,omitempty"`
	NFTCount   int          `json:"nftCount,omitempty"`
	Warnings   []string     `json:"warnings,omitempty"`
	Meta       []core.KV    `json:"meta,omitempty"`
	ValueUSD   float64      `json:"valueUsd"`
	Status     Status       `json:"status"`
	Err        string       `json:"error,omitempty"`
	AsOf       time.Time    `json:"asOf"`
}

// HoldingRow is a holding with its price applied.
type HoldingRow struct {
	core.Holding
	Price    *core.Price `json:"price,omitempty"`
	ValueUSD float64     `json:"valueUsd"`
	Priced   bool        `json:"priced"`
}

// Point is one chart sample.
type Point struct {
	T time.Time `json:"t"`
	V float64   `json:"v"`
}

// Change is a value change over a window.
type Change struct {
	Abs   float64 `json:"abs"`
	Pct   float64 `json:"pct"`
	Valid bool    `json:"valid"` // false when history does not cover the window yet
	Label string  `json:"label"` // "past week"
}
