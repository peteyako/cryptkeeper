package tax

import "time"

// Untracked is the pseudo-wallet that holds coins whose origin is outside the tracked
// history. It exists only for tax purposes.
const Untracked = "untracked"

// dust is the unit amount below which a lot counts as fully consumed. It absorbs
// floating-point residue so a full disposal leaves exactly zero.
const dust = 1e-12

// Term is the holding-period class of a disposal.
type Term string

const (
	ShortTerm Term = "short"
	LongTerm  Term = "long"
)

// Tag classifies a transaction for tax purposes.
type Tag string

const (
	TagPurchase     Tag = "purchase"
	TagIncome       Tag = "income"
	TagAirdrop      Tag = "airdrop"
	TagGiftReceived Tag = "gift-received"
	TagOwnWallet    Tag = "own-wallet"
	TagPosition     Tag = "position"
	TagSpam         Tag = "spam"
	TagSale         Tag = "sale"
	TagGift         Tag = "gift"
	TagLost         Tag = "lost"
)

// Lot flags mark lots whose basis or date is not firm.
const (
	FlagMissingBasis = "missing-basis"
	FlagTransferDate = "transfer-date"
)

// Lot is a quantity of one asset in one wallet with its cost.
type Lot struct {
	Wallet   string    `yaml:"wallet" json:"wallet"`
	Asset    string    `yaml:"asset" json:"asset"`
	Amount   float64   `yaml:"amount" json:"amount"`
	CostUSD  float64   `yaml:"costUsd" json:"costUsd"`
	Acquired time.Time `yaml:"acquired" json:"acquired"`
	// Various means "acquired on various dates up to Acquired" and prints as VARIOUS.
	Various bool   `yaml:"various" json:"various"`
	TxID    string `yaml:"txId" json:"txId"`
	Flag    string `yaml:"flag" json:"flag"`
}

// Disposal is one lot (or part of one) sold, spent, gifted or lost.
type Disposal struct {
	Sold        time.Time `yaml:"sold" json:"sold"`
	Acquired    time.Time `yaml:"acquired" json:"acquired"`
	Various     bool      `yaml:"various" json:"various"`
	Asset       string    `yaml:"asset" json:"asset"`
	Amount      float64   `yaml:"amount" json:"amount"`
	CostUSD     float64   `yaml:"costUsd" json:"costUsd"`
	ProceedsUSD float64   `yaml:"proceedsUsd" json:"proceedsUsd"`
	GainUSD     float64   `yaml:"gainUsd" json:"gainUsd"`
	Wallet      string    `yaml:"wallet" json:"wallet"`
	Term        Term      `yaml:"term" json:"term"`
	Note        string    `yaml:"note" json:"note"`
	TxID        string    `yaml:"txId" json:"txId"`
}

// Income is a receipt taxed as income at its fair market value.
type Income struct {
	Date     time.Time `yaml:"date" json:"date"`
	Asset    string    `yaml:"asset" json:"asset"`
	Amount   float64   `yaml:"amount" json:"amount"`
	ValueUSD float64   `yaml:"valueUsd" json:"valueUsd"`
	// Type is "reward", "income", "airdrop" or "position".
	Type   string `yaml:"type" json:"type"`
	Wallet string `yaml:"wallet" json:"wallet"`
	TxID   string `yaml:"txId" json:"txId"`
}

// Parked is an amount of an asset a wallet holds inside a position (locked, posted as
// collateral): out of its lots but not disposed.
type Parked struct {
	Wallet string  `yaml:"wallet"`
	Asset  string  `yaml:"asset"`
	Amount float64 `yaml:"amount"`
}

// IssueKind names a problem the engine could not resolve on its own.
type IssueKind string

const (
	IssueUnmatchedReceive IssueKind = "unmatched-receive"
	IssueUnmatchedSend    IssueKind = "unmatched-send"
	IssueMissingPrice     IssueKind = "missing-price"
	IssueMissingBasis     IssueKind = "missing-basis"
	IssuePositionExcess   IssueKind = "position-excess"
	IssueTransferUnpaired IssueKind = "transfer-unpaired"
)

// Issue is a data problem the user should review. Amount is always positive: the size of
// the leg or shortfall concerned, whatever its direction.
type Issue struct {
	Kind   IssueKind `yaml:"kind" json:"kind"`
	TxID   string    `yaml:"txId" json:"txId"`
	Wallet string    `yaml:"wallet" json:"wallet"`
	Asset  string    `yaml:"asset" json:"asset"`
	Amount float64   `yaml:"amount" json:"amount"`
	Time   time.Time `yaml:"time" json:"time"`
}
