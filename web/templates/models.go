package templates

import (
	"time"

	"github.com/a-h/templ"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/portfolio"
)

// Shell is what every full page needs to draw the app chrome.
type Shell struct {
	Title      string
	Active     string // "overview","wallets","activity","reports","alerts","settings"
	Portfolios []PortfolioNav
	PF         string // current portfolio id or "all"
	Range      string // "1D","1W","1M","1Y","All"
	ShowLogos  bool
	AuthUser   string // "" when auth is off
}

// PortfolioNav is one entry of the sidebar "Portfolios" list.
type PortfolioNav struct{ ID, Name, Count string }

// SyncInfo drives the sync chip, e.g. synced/"Synced 2m ago", stale/"Stale 3h",
// error/"2 sources failing", pending/"Syncing…".
type SyncInfo struct {
	Status portfolio.Status
	Label  string
}

// WatchRow is one line of the "Watching" table.
type WatchRow struct {
	AssetKey, Symbol, Name string
	Price                  *core.Price
}

// OverviewPage is the portfolio overview (Portfolio.dc.html).
type OverviewPage struct {
	Shell
	PortfolioName string
	Snap          portfolio.Snapshot
	Chart         []portfolio.Point
	Change        portfolio.Change
	Sync          SyncInfo
	Watch         []WatchRow
	NetWorthShare float64 // 0..1, shown only when PF != "all"
	Now           time.Time
}

// WalletsPage lists every source as an account card.
type WalletsPage struct {
	Shell
	Wallets []portfolio.WalletView
	Sync    SyncInfo
	Now     time.Time
}

// WalletPage is the wallet detail (Wallet.dc.html).
type WalletPage struct {
	Shell
	W          portfolio.WalletView
	Tab        string // "holdings","activity","addresses","utxos"
	Addresses  []core.Address
	UTXOs      []core.UTXO
	Receive    string
	ReceiveQR  templ.Component // nil if none; an SVG component supplied by the handler
	IsBitcoin  bool
	CostBasis  *float64 // nil until the P&L milestone: the tile shows "—" and "Set cost basis"
	Unrealized *float64
	Now        time.Time
}

// SourcePickPage is step 1 of the add-source flow.
type SourcePickPage struct{ Shell }

// SourceFormPage is step 2 of the add-source flow, or the edit form.
type SourceFormPage struct {
	Shell
	Type        string            // "coinbase","kraken","bitcoin","solana","manual"
	EditID      string            // "" when adding
	Values      map[string]string // non-secret field values (label, kind, key, address, keyName, holdings...)
	SecretMasks map[string]string // field name -> "••••3f9a" when editing; secret inputs always render empty
	Errors      map[string]string // field name -> message; "" key = form-level
}

// TestResult is the outcome of "Test connection".
type TestResult struct {
	OK      bool
	Message string
	Rows    []portfolio.HoldingRow
}

// SettingsPage is the settings screen.
type SettingsPage struct {
	Shell
	Wallets     []portfolio.WalletView
	DataDir     string
	AuthEnabled bool
	Version     string
}
