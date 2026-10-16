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
	Active     string // "overview","wallets","activity","reports","alerts","settings","portfolios"
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
	// PnL is the unrealized profit or loss per merged asset key. It feeds the optional
	// P/L column of the holdings table; with no entries the toggle is not shown.
	PnL map[string]PnLCell
	Now time.Time
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
	CostBasis  *float64 // nil when unknown: the tile shows "—" and "Set cost basis"
	Unrealized *float64
	// Activity is the wallet's most recent entries (up to 25) for the Activity tab, and
	// ActivityMore says there are older ones (the tab then links to the full Activity page).
	Activity     []ActivityRow
	ActivityMore bool
	// PublicEndpoint and RetryEvery explain a balance error (W.Err): whether the source reads
	// from a shared public endpoint, and how often balances refresh.
	PublicEndpoint bool
	RetryEvery     time.Duration
	Now            time.Time
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
	// Sync is the per-source activity sync state shown in the "Activity sync" card.
	Sync []SyncRow
	// Now times the relative "2m ago" texts; the zero value means time.Now().
	Now time.Time
	// BackupImport is the outcome of the last restore (Settings → Backup & restore).
	BackupImport ImportOutcome
	// EncryptedExports: CRYPTKEEP_SECRET_KEY is set, so exports are encrypted as a whole.
	EncryptedExports bool
	// PriceKey is the saved CoinGecko key, masked ("••••abcd"); empty when none is saved.
	PriceKey string
	// PriceKeyFromEnv: COINGECKO_API_KEY is set, so the Settings form is disabled.
	PriceKeyFromEnv bool
}

// PortfolioItem is one portfolio on the manager page.
type PortfolioItem struct {
	ID, Name    string
	ValueUSD    float64
	Share       float64  // 0..1 of net worth
	WalletsText string   // "All wallets" | "3 wallets" | "No wallets"
	AssetsText  string   // "All assets" | "BTC, SOL +2" (first 3 symbols, then "+N")
	WatchCount  int      // assets on the watch list
	Missing     []string // explicit wallet ids that no longer exist (shown as a warning)
}

// BackupRow is one automatic backup of the portfolio list.
type BackupRow struct {
	Name string
	Time time.Time
	Size int64
}

// ImportOutcome is the result of an import (or restore), e.g. "Imported: 2 added, 1
// replaced, 3 unchanged." A zero value renders nothing.
type ImportOutcome struct {
	OK      bool
	Message string
}

// PortfoliosPage is the portfolio manager (/portfolios).
type PortfoliosPage struct {
	Shell
	Items     []PortfolioItem
	Backups   []BackupRow
	LoadError string // non-empty when data/portfolios.yaml has an invalid hand edit
	Now       time.Time
	// Import is the result shown under the import form. Optional: a handler can also
	// patch ImportResultView on its own.
	Import ImportOutcome
	// EncryptedExports: CRYPTKEEP_SECRET_KEY is set, so exports are encrypted as a whole.
	EncryptedExports bool
}

// WalletOption is one wallet a portfolio can be limited to.
type WalletOption struct {
	ID, Label, Type string
	Kind            core.Kind
}

// AssetOption is one currently held asset a portfolio can be limited to.
type AssetOption struct {
	Key, Symbol, Name string
	ValueUSD          float64
}

// PortfolioFormPage is the create / edit portfolio form.
type PortfolioFormPage struct {
	Shell
	EditID          string // "" when creating
	Name            string
	AllWallets      bool
	SelectedWallets map[string]bool
	AllAssets       bool
	SelectedAssets  map[string]bool // asset keys
	ExtraAssets     string          // asset keys not currently held, space or comma separated
	Watch           string          // watch keys, space or comma separated
	WalletOptions   []WalletOption
	AssetOptions    []AssetOption     // currently held assets, by value descending
	Errors          map[string]string // field -> message; "" = form level
}

// ---- activity ---------------------------------------------------------------------------

// ActivityRow is one entry of the activity feed with the labels the template cannot look up.
type ActivityRow struct {
	Tx          core.Tx
	SourceLabel string
	SourceKind  core.Kind
	PeerLabel   string // other side of a matched transfer ("Cold storage")
}

// SyncRow is one source's activity sync state.
type SyncRow struct {
	ID, Label    string
	Type         string // source type: names the service in an error
	Kind         core.Kind
	Count        int
	BackfillDone bool
	Syncing      bool // a sync of this source is running right now
	LastSync     time.Time
	LastError    string // raw error of the most recent failed attempt
	// PublicEndpoint is set when the source reads from a shared public endpoint (the public
	// Solana RPC, mempool.space), the usual cause of timeouts and rate limits.
	PublicEndpoint bool
	Every          time.Duration // how often activity syncs, i.e. when a failure is retried
}

// ActivityPage is the Activity screen (/activity).
type ActivityPage struct {
	Shell
	Rows     []ActivityRow
	More     string // opaque "before" cursor for the next page; "" = no more
	SourceID string // current filters ("" = all)
	AssetKey string
	Kind     string
	// Period filter: Period is the key ("", 7d, 30d, 90d, mtd, ytd, y2025, custom); From/To
	// are the custom dates (YYYY-MM-DD); PeriodLabel describes it ("Year to date").
	Period, From, To, PeriodLabel string
	Years                         []int          // years that have activity, newest first (period options)
	Sources                       []WalletOption // filter options
	Assets                        []AssetOption
	Sync                          []SyncRow
	Now                           time.Time
}

// ---- reports ----------------------------------------------------------------------------

// PnLRow is one asset of the profit and loss table.
type PnLRow struct {
	AssetKey, Symbol, Name string
	Amount, Value          float64
	AvgCost, Basis         float64
	Unrealized             float64
	UnrealizedPct          float64
	HasBasis, Overridden   bool
	UnknownUnits           float64 // units without a known cost
}

// YearAmount is realized profit or loss for one calendar year.
type YearAmount struct {
	Year int
	USD  float64
}

// ManualBasisRow is a cost entered by hand: Amount units for CostUSD in total.
type ManualBasisRow struct {
	Asset           string
	Amount, CostUSD float64
	Note            string
}

// ReportsPage is the Reports screen (/reports).
type ReportsPage struct {
	Shell
	Rows                                    []PnLRow
	TotalBasis, TotalValue, TotalUnrealized float64
	Coverage                                float64      // 0..1 share of value with a known cost
	Realized                                []YearAmount // newest year first
	Manual                                  []ManualBasisRow
	AssetOptions                            []AssetOption
	Import                                  ImportOutcome // CSV import result
	BasisErr                                string        // manual cost form error
	Now                                     time.Time
}

// PnLCell is the unrealized result of one asset, for the overview's P/L column.
type PnLCell struct {
	Unrealized, Pct float64
	OK              bool
}

// ---- alerts -----------------------------------------------------------------------------

// AlertRuleRow is one alert rule in the list.
type AlertRuleRow struct {
	ID, Name, Type, Summary string
	Enabled                 bool
}

// AlertLogRow is one fired (or failed) notification.
type AlertLogRow struct {
	Time                     time.Time
	RuleID, Title, Body, Err string
}

// AlertsPage is the Alerts screen (/alerts).
type AlertsPage struct {
	Shell
	Rules     []AlertRuleRow
	Recent    []AlertLogRow
	NtfyHost  string // "" = ntfy is not configured
	Redact    bool   // amounts are hidden in notifications
	LoadError string // alerts.yaml has an invalid hand edit
	Test      ImportOutcome
	Now       time.Time
}

// AlertFormPage is the create / edit alert form.
type AlertFormPage struct {
	Shell
	EditID           string
	Values           map[string]string // id, name, type, asset, above, below, portfolio, pct, window, minutes, minUsd, minPct, cooldown, enabled
	SelectedWallets  map[string]bool
	WalletOptions    []WalletOption
	PortfolioOptions []PortfolioNav // includes {ID: "all", Name: "All holdings"}
	AssetOptions     []AssetOption
	Errors           map[string]string // field -> message; "" = form level
}
