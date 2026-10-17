package demo

import (
	"strconv"
	"time"

	"crypto-portfolio/web/templates"
)

// Koinly returns the Koinly import wizard mid-import, with invented figures: last year's
// holdings, transaction history and capital gains loaded, the wallets and assets mapped
// (one ambiguous symbol left to the user), and a reconciliation with differences to check.
func Koinly(now time.Time) templates.KoinlyPage {
	year := now.Year() - 1
	var wallets []templates.KoinlyOption
	for _, w := range Snapshot(now).Wallets {
		wallets = append(wallets, templates.KoinlyOption{Value: w.ID, Label: templates.KoinlyWalletLabel(w.Label, w.Type)})
	}
	wallets = append(wallets, templates.KoinlyOption{Value: "untracked", Label: "Untracked (no longer synced)"})
	w := templates.KoinlyWizard{
		HasDraft: true, Year: year, AsOf: time.Date(year, 12, 31, 23, 59, 0, 0, time.UTC),
		Files:    []string{"koinly_end_of_year_holdings.csv", "koinly_transaction_history.csv", "koinly_capital_gains.csv"},
		Kinds:    []string{"holdings", "transactions", "gains"},
		Holdings: 4, Txns: 212, Gains: 37,
		Outcome: templates.ImportOutcome{OK: true, Message: "Loaded koinly_capital_gains.csv (capital gains) for " + strconv.Itoa(year) + "."},
		Wallets: []templates.KoinlyMapRow{
			{Name: "Ledger", Selected: IDCold},
			{Name: "Phantom main", Selected: IDPhantom},
			{Name: "Old exchange account", Selected: "untracked"},
		},
		WalletOptions: wallets,
		Assets: []templates.KoinlyMapRow{
			{Name: "BTC (Bitcoin)", Selected: "BTC", Symbol: "BTC", Qty: 0.35, Cost: 13250},
			{Name: "SOL (Solana)", Selected: "SOL", Symbol: "SOL", Qty: 41.5, Cost: 5100},
			{Name: "JUP (Jupiter)", Selected: "", Symbol: "JUP", Qty: 900, Cost: 610,
				Note: "Several assets use the symbol JUP: choose the right one.", ConfirmSkip: true, SkipOK: true},
			{Name: "FREE (Claim at example.invalid)", Selected: "", Symbol: "FREE", Qty: 1000},
		},
		AssetOptions: []templates.KoinlyOption{
			{Value: "", Label: "Skip"}, {Value: "BTC", Label: "BTC · Bitcoin"}, {Value: "JUP", Label: "JUP · Jupiter"},
			{Value: "SOL", Label: "SOL · Solana"},
		},
		Recon: []templates.KoinlyReconRow{
			{KoinlyAsset: "BTC (Bitcoin)", AssetKey: "BTC", Symbol: "BTC", KoinlyQty: 0.35, KoinlyCost: 13250,
				CryptKeepQty: 0.30006114, Dated: 0.05, Various: 0.25006114, Untracked: 0.04993886},
			{KoinlyAsset: "SOL (Solana)", AssetKey: "SOL", Symbol: "SOL", KoinlyQty: 41.5, KoinlyCost: 5100,
				CryptKeepQty: 42.75, Dated: 12, Various: 29.5, MissingBasis: 1.25, ExtraBasis: 212.5},
		},
		Problems: []string{
			"activity history for Kraken doesn't reach " + strconv.Itoa(year+1) + "-01-01; its quantity may be off",
			"JUP (Jupiter): skipped; Koinly's $610.00 cost won't carry over",
			"BTC: Koinly 0.35 vs CryptKeep 0.30006114",
			"SOL: Koinly 41.5 vs CryptKeep 42.75",
		},
	}
	return templates.KoinlyPage{Shell: Shell("Import from Koinly", "reports", "all", ""), Wizard: w}
}

// KoinlyEmpty is the wizard before any upload: just the upload step.
func KoinlyEmpty(now time.Time) templates.KoinlyPage {
	return templates.KoinlyPage{Shell: Shell("Import from Koinly", "reports", "all", "")}
}

// KoinlyBlocked is the wizard with only the transaction history loaded: nothing to
// reconcile, and Accept waits for the holdings report. The last accept was refused.
func KoinlyBlocked(now time.Time) templates.KoinlyPage {
	p := Koinly(now)
	w := &p.Wizard
	w.Files, w.Kinds = w.Files[1:2], []string{"transactions"}
	w.AsOf, w.Holdings, w.Gains = time.Time{}, 0, 0
	w.Outcome = templates.ImportOutcome{Message: "Nothing was loaded: coinbase.csv: not a Koinly report."}
	w.Assets, w.Recon, w.Problems = nil, nil, nil
	w.Blocked = "Upload the end of year holdings report first: it has the quantities and costs the year closes with."
	w.AcceptOutcome = templates.ImportOutcome{Message: w.Blocked}
	return p
}
