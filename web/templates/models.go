package templates

import (
	"time"

	"github.com/a-h/templ"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/portfolio"
	"cryptkeeper/internal/tax"
	"cryptkeeper/internal/tax/report"
)

// Shell is what every full page needs to draw the app chrome.
type Shell struct {
	Title      string
	Active     string // "overview","wallets","activity","reports","alerts","settings","portfolios"
	Portfolios []PortfolioNav
	PF         string // current portfolio id or "all"
	Range      string // "1D","1W","1M","YTD","1Y","All"
	ShowLogos  bool
	AuthUser   string // "" when auth is off
}

// PortfolioNav is one entry of the sidebar "Portfolios" list.
type PortfolioNav struct{ ID, Name, Count string }

// SyncInfo drives one sync chip, e.g. synced/"Prices just now", stale/"Wallets stale 3h ago",
// error/"2 sources failing", pending/"Syncing wallets…". Title is its tooltip.
type SyncInfo struct {
	Status portfolio.Status
	Label  string
	Title  string
}

// SyncStatus drives the header's two sync chips. Prices refresh more often than wallet
// balances, so each says on its own when it last came in.
type SyncStatus struct{ Prices, Wallets SyncInfo }

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
	Sync          SyncStatus
	Watch         []WatchRow
	NetWorthShare float64 // 0..1, shown only when PF != "all"
	// PnL is the unrealized profit or loss per merged asset key. It feeds the optional
	// P/L column of the holdings table; with no entries the toggle is not shown.
	PnL map[string]PnLCell
	// Trends is each asset's prices over the last TrendWindow, oldest first and ending at
	// the current price: the holdings table's price lines. An asset without two has none.
	Trends map[string][]portfolio.Point
	Now    time.Time
	// BackfillDays > 0 offers "Load last N days" under the chart (0 = no offer).
	BackfillDays int
	// BackfillErr is the note shown when loading history failed (a whole sentence).
	BackfillErr string
	// BackfillNote is the note shown when loading history could add nothing.
	BackfillNote string
}

// WalletsPage lists every source as an account card.
type WalletsPage struct {
	Shell
	Wallets []portfolio.WalletView
	Sync    SyncStatus
	Now     time.Time
}

// WalletPage is the wallet detail (Wallet.dc.html).
type WalletPage struct {
	Shell
	W         portfolio.WalletView
	Tab       string // "holdings","activity","addresses","utxos"
	Addresses []core.Address
	UTXOs     []core.UTXO
	Receive   string
	ReceiveQR templ.Component // nil if none; an SVG component supplied by the handler
	IsBitcoin bool
	CostBasis *float64 // from the wallet's tax lots; nil when unknown: the tile shows "—"
	// NoTax is set for a wallet the tax scope (Reports) leaves out: it has no cost basis.
	NoTax      bool
	Unrealized *float64
	// CostUnits is how many units CostBasis covers (units in missing-basis lots, or with
	// no lot at all, are left out), so the tile's avg is CostBasis / CostUnits.
	CostUnits float64
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
	Type        string            // "coinbase","kraken","bitcoin","solana","manual","brokerage"
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
	// EncryptedExports: CRYPTKEEPER_SECRET_KEY is set, so exports are encrypted as a whole.
	EncryptedExports bool
	// PriceKey is the saved CoinGecko key, masked ("••••abcd"); empty when none is saved.
	PriceKey string
	// PriceKeyFromEnv: COINGECKO_API_KEY is set, so the Settings form is disabled.
	PriceKeyFromEnv bool
	// BlockstreamID is the saved Blockstream API key's client ID, masked; empty when none is saved.
	BlockstreamID string
	// BlockstreamFromEnv: BLOCKSTREAM_CLIENT_ID is set, so the Settings form is disabled.
	BlockstreamFromEnv bool
	// RefreshMinutes, PriceMinutes and ActivityMinutes are how often balances and prices
	// refresh and activity syncs; prices at most as seldom as balances.
	RefreshMinutes, PriceMinutes, ActivityMinutes int
	// BlockstreamNote is the outcome of the last save ("" on a plain page load).
	BlockstreamNote    string
	BlockstreamNoteErr bool
}

// PortfolioItem is one portfolio on the manager page.
type PortfolioItem struct {
	ID, Name    string
	ValueUSD    float64
	Share       float64  // 0..1 of net worth
	StableShare float64  // 0..1 of the value held as cash or stablecoins
	HasValue    bool     // StableShare is meaningful (something is valued)
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
	// EncryptedExports: CRYPTKEEPER_SECRET_KEY is set, so exports are encrypted as a whole.
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

// PnLRow is one asset of the profit and loss table, costed from the open tax lots.
type PnLRow struct {
	AssetKey, Symbol, Name string
	Amount, Value          float64
	// Basis is the cost of the units covered by lots with a known cost; AvgCost is that
	// basis per covered unit.
	AvgCost, Basis float64
	Unrealized     float64
	UnrealizedPct  float64
	HasBasis       bool
	UnknownUnits   float64 // units held that no lot with a known cost covers
	MissingUnits   float64 // units in missing-basis lots (held beyond what the import had a cost for)
}

// YearAmount is realized profit or loss for one tax year.
type YearAmount struct {
	Year int
	USD  float64
}

// ReportsPage is the Reports screen (/reports).
type ReportsPage struct {
	Shell
	Rows                                    []PnLRow
	TotalBasis, TotalValue, TotalUnrealized float64
	Coverage                                float64       // 0..1 share of value with a known cost
	Realized                                []YearAmount  // newest year first
	Import                                  ImportOutcome // CSV import result
	TaxErr                                  string        // data/tax.yaml failed to reload
	// MissingBasisSales is how many open-year sales had no cost basis (the realized card
	// then points at the Koinly import).
	MissingBasisSales int
	Now               time.Time
	Tax               TaxPage    // the Tax section (its Needs review tab hosts Review)
	Review            ReviewPage // the "Needs review" panel, shown in the Tax section's review tab
}

// TaxPageSize is how many capital-gains rows one page of the Tax section lists.
const TaxPageSize = 200

// TaxPage is the Tax section of Reports (#tax-section): a year selector, the year's tiles
// and one tab of its report.
type TaxPage struct {
	Years  []int
	Year   int
	Tab    string // "gains" | "income" | "holdings" | "review"
	Page   int    // 1-based page of the gains tab
	Pages  int
	Report report.YearReport
	// ReviewTab is whether the Needs review tab exists: the latest open year only.
	ReviewTab bool
	Review    ReviewPage // set for the review tab
	// Symbols and Wallets name the assets and wallets of the income rows (asset key and
	// wallet ID to display text).
	Symbols, Wallets map[string]string
	// ZoneNote warns that the open year starts at midnight in the timezone the last year was
	// closed in, not in the current tax timezone; "" when they agree.
	ZoneNote string
	// Err is shown instead of the report when it could not be built.
	Err string
	// ScopeOptions are the choices of what an open year's tax covers ("" = all wallets, else
	// a portfolio ID); Scope is the chosen one and ScopeName its portfolio's name.
	// ScopeNote explains a saved scope that no longer applies.
	ScopeOptions     []ScopeOption
	Scope, ScopeName string
	ScopeNote        string
}

// ScopeOption is one choice of what the tax covers: a portfolio, or all wallets (ID "").
type ScopeOption struct{ ID, Name string }

// ---- Needs review -----------------------------------------------------------------------

// ReviewTagged is the Kind of the collapsed group of already-tagged transactions.
const ReviewTagged tax.IssueKind = "tagged"

// ReviewPage is the "Needs review" panel: the tax engine's open issues grouped by kind and
// asset, what is parked in positions, and tags that point at transactions that no longer
// exist. Its root is #review-panel.
type ReviewPage struct {
	Groups   []ReviewGroup
	Parked   []ParkedRow
	Stale    []StaleTag
	OpenYear int
	// WriteOffs are the saved write-offs, in tax.yaml order (Index is the position).
	WriteOffs []WriteOffRow
	// Err is the message shown at the top after a refused action.
	Err string
	// Close is the close/reopen card at the top of the panel.
	Close CloseCard
	// UndatedLots counts manual lots without a date; they are ignored once a year is closed.
	UndatedLots int
	// EarlyLots counts, once a year is closed, the manual lots dated before OpenStart: the
	// closed years hold them, so they are ignored.
	EarlyLots int
	// OpenStart is the first day of the open year as shown ("Jan 2, 2006"); set with
	// EarlyLots.
	OpenStart string
	// TagsChecked is false when the activity could not be loaded: the Tagged and stale
	// sections are then left out.
	TagsChecked bool
}

// ReviewGroup is the issues of one kind for one asset. Count is all of them; Items is the
// newest few (the rest is "+N more").
type ReviewGroup struct {
	Kind          tax.IssueKind
	Title         string
	Asset, Symbol string
	Count         int
	Items         []ReviewItem
}

// ReviewItem is one transaction to review. Direction is "in" or "out" ("" when the
// transaction is unknown). Amount is formatted; PriceKey is set for a missing price.
type ReviewItem struct {
	TxID, WalletLabel, Time, Direction, Asset, Symbol, Amount string
	USD                                                       *float64
	Explorer, Tag, Note, PriceKey                             string
	TagOptions                                                []string
}

// ParkedRow is an amount a wallet holds inside a position.
type ParkedRow struct {
	Wallet, WalletLabel, Asset, Symbol string
	Amount                             float64
}

// WriteOffRow is a saved write-off. Wallet, Asset, At (RFC 3339) and AmountKey identify it
// to the delete action, which refuses when the write-off at Index is no longer this one.
type WriteOffRow struct {
	Index                     int
	Wallet, Asset             string
	WalletLabel, Symbol       string
	Amount, ProceedsUSD       float64
	Date, Note, At, AmountKey string
}

// StaleTag is a tag in tax.yaml whose transaction is gone.
type StaleTag struct {
	TxID string
	Tag  tax.Tag
}

// ---- Koinly import ----------------------------------------------------------------------

// KoinlyPage is the Koinly import wizard (/reports/koinly).
type KoinlyPage struct {
	Shell
	Wizard KoinlyWizard
}

// KoinlyWizard is the wizard's steps: upload, map, reconcile and accept. Its root is
// #koinly-wizard, which every step's POST patches.
type KoinlyWizard struct {
	Outcome       ImportOutcome // the last upload's result, shown under the upload form
	AcceptOutcome ImportOutcome // a refused accept, shown under the Accept button
	// HasDraft: files were uploaded. The mapping and reconciliation steps show only then.
	HasDraft bool
	Year     int
	AsOf     time.Time // the holdings report's date; zero without one
	Files    []string  // uploaded file names
	Kinds    []string  // the report kinds uploaded: "holdings", "transactions", "gains", "income"
	// Rows loaded from each report.
	Holdings, Txns, Gains, Income int
	Wallets                       []KoinlyMapRow // Koinly wallet name → source ID | "untracked"
	WalletOptions                 []KoinlyOption
	Assets                        []KoinlyMapRow // Koinly "SYM (Name)" → asset key | "" (skip)
	AssetOptions                  []KoinlyOption
	Recon                         []KoinlyReconRow
	// Problems are the differences and warnings the user must read before accepting.
	Problems []string
	// Blocked says why Accept is not possible yet; "" when it is.
	Blocked string
}

// KoinlyOption is one choice of a mapping select.
type KoinlyOption struct{ Value, Label string }

// KoinlyMapRow is one Koinly wallet or asset and what it maps to. Asset rows also carry
// Koinly's year-end quantity and cost, and Note (e.g. the candidates of an ambiguous
// symbol). ConfirmSkip shows the "Skip anyway" box (an ambiguous asset with a cost left on
// Skip blocks Accept until it is ticked); SkipOK is its state.
type KoinlyMapRow struct {
	Name, Selected      string
	Symbol              string
	Qty, Cost           float64
	Note                string
	ConfirmSkip, SkipOK bool
}

// KoinlyReconRow is one mapped asset's reconciliation: Koinly's year-end quantity and cost
// against CryptKeeper's quantity at the cutover, and where the units went.
type KoinlyReconRow struct {
	KoinlyAsset, AssetKey, Symbol         string
	KoinlyQty, KoinlyCost, CryptKeeperQty float64
	// Dated: units dated from the year's acquisitions; Various: units from before the
	// year; Untracked: Koinly's excess, in no synced wallet; MissingBasis: CryptKeeper's
	// excess, with no Koinly cost.
	Dated, Various, Untracked, MissingBasis float64
	ExtraBasis                              float64 // the user's total USD basis for MissingBasis units
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

// CloseCard is the close/reopen year card. Confirm is "" (idle), "close" or "reopen" (asking
// to confirm Year, with Msg). CloseYear / ReopenYear are the years the idle card offers (0 =
// none).
type CloseCard struct {
	Confirm    string
	Year       int
	Msg        string
	CloseYear  int
	ReopenYear int
}

// CoinWallet is one source's holding of the coin on its page.
type CoinWallet struct {
	ID, Label        string
	Amount, ValueUSD float64
}

// CoinRanges are the coin page's chart ranges and the days each one loads.
var CoinRanges = []string{"7D", "30D", "90D", "1Y"}

// CoinDays is the number of daily points per coin range.
var CoinDays = map[string]int{"7D": 7, "30D": 30, "90D": 90, "1Y": 365}

// CoinPage is the detail page of one asset: its price, where it is held and an on-demand
// price chart.
type CoinPage struct {
	Shell
	AssetKey, Symbol, Name, LogoURL string
	Price                           *core.Price
	Amount, ValueUSD                float64
	Wallets                         []CoinWallet
	Range                           string // "7D","30D","90D","1Y"
	Points                          []portfolio.Point
	Change                          portfolio.Change // over the chart range, from Points
	Loading                         bool             // chart not loaded yet: shows a note and loads itself
	Sync                            SyncStatus
	Now                             time.Time
}

// CoinWalletsOf is the sources holding the asset, with their summed amount and value;
// sources with a zero balance are left out.
func CoinWalletsOf(ws []portfolio.WalletView, key string) []CoinWallet {
	var out []CoinWallet
	for _, w := range ws {
		cw := CoinWallet{ID: w.ID, Label: w.Label}
		for _, h := range w.Rows {
			if h.AssetKey == key {
				cw.Amount += h.Amount
				cw.ValueUSD += h.ValueUSD
			}
		}
		if cw.Amount != 0 {
			out = append(out, cw)
		}
	}
	return out
}
