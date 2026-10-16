package core

import (
	"context"
	"time"
)

// TxKind classifies an activity entry.
type TxKind string

const (
	TxReceive  TxKind = "receive"
	TxSend     TxKind = "send"
	TxTransfer TxKind = "transfer" // between two of the user's own sources, or a self-send
	TxTrade    TxKind = "trade"    // buy/sell/convert on an exchange
	TxSwap     TxKind = "swap"     // on-chain swap (2+ legs with opposite signs)
	TxReward   TxKind = "reward"   // staking/earn/interest income
	TxStake    TxKind = "stake"    // stake/unstake moves
	TxFee      TxKind = "fee"
	TxPosition TxKind = "position" // moved into or out of one of the user's own positions (locked JUP, perps collateral)
	TxOther    TxKind = "other"
)

// TxStatus is the settlement state of an entry.
type TxStatus string

const (
	TxPending   TxStatus = "pending"
	TxConfirmed TxStatus = "confirmed"
	TxFailed    TxStatus = "failed"
)

// Leg is one asset movement inside an entry. Amount is signed from the user's point of
// view: positive = into the source, negative = out of it.
type Leg struct {
	AssetKey string   `json:"assetKey"`
	Symbol   string   `json:"symbol"`
	Mint     string   `json:"mint,omitempty"`
	Amount   float64  `json:"amount"`
	USD      *float64 `json:"usd,omitempty"` // value at the time, when known (signed like Amount)
}

// Tx is one activity entry for one source.
type Tx struct {
	// ID is unique across sources: SourceID + ":" + ExternalID.
	ID            string    `json:"id"`
	SourceID      string    `json:"sourceId"`
	ExternalID    string    `json:"externalId"` // txid, signature, Coinbase tx id, Kraken refid
	Time          time.Time `json:"time"`
	Kind          TxKind    `json:"kind"`
	Status        TxStatus  `json:"status"`
	Confirmations int       `json:"confirmations,omitempty"`
	Legs          []Leg     `json:"legs"`
	Fee           *Leg      `json:"fee,omitempty"` // negative amount, already excluded from Legs
	Counterparty  string    `json:"counterparty,omitempty"`
	TxHash        string    `json:"txHash,omitempty"` // on-chain hash, used to match transfers between sources
	ExplorerURL   string    `json:"explorerUrl,omitempty"`
	Note          string    `json:"note,omitempty"`
	// Peer links the other side of a transfer between two of the user's sources
	// (filled by the activity package's transfer matcher, not by sources).
	PeerID     string `json:"peerId,omitempty"`
	PeerSource string `json:"peerSource,omitempty"`
}

// ActivityRequest asks a source for activity. State is opaque: whatever the source returned
// last time (nil on the first call), so each source keeps its own cursors (per address,
// per account, per ledger offset). Budget bounds the API requests one call may make;
// sources do new activity first, then backfill older history down to Since.
type ActivityRequest struct {
	State  []byte
	Since  time.Time
	Budget int
}

// ActivityResult carries new or updated entries (e.g. pending → confirmed) and the next state.
type ActivityResult struct {
	Txs          []Tx
	State        []byte
	BackfillDone bool // history down to Since has been fetched
}

// ActivitySource is implemented by sources that can list transactions.
//
// On failure a source may return an error together with a result: if the result carries a
// non-nil State, the entries and State reflect valid progress up to the failure and are
// saved (the error is still reported). An error with a nil State discards the result.
type ActivitySource interface {
	Activity(ctx context.Context, req ActivityRequest) (ActivityResult, error)
}

// TxID builds the cross-source unique id.
func TxID(sourceID, externalID string) string { return sourceID + ":" + externalID }
