package solana

import "context"

// Jupiter's limit orders (Trigger v2) keep an open order's funds in a vault: a plain key
// Jupiter holds for the wallet, so nothing derives it from the wallet's address. But Jupiter
// signs with the vault next to one of its own keys, which shows in the wallet's history.
const (
	// jupiterTriggerSigner co-signs every deposit from a wallet into its vault.
	jupiterTriggerSigner = "trfb53BmkHNeoqaa3REgqnrbwUZqAFYdjTkivkJ6aWg"
	// jupiterGasPayer pays for the fills and withdrawals a vault signs.
	jupiterGasPayer = "gasAx5Y917MYdmdnwiomwYDhmDKNGDJnN1MmEbxVdVw"
)

// FindJupiterVault looks for the wallet's Jupiter limit-order vault in its transactions:
// first the signatures in try (likely ones, such as the transfers Activity has), then its
// history, newest first, reading at most budget transactions in all. It returns "" and no
// error when none of them shows a vault. progress, when set, is told how many it has read.
func (s *Source) FindJupiterVault(ctx context.Context, try []string, budget int, progress func(read int)) (string, error) {
	seen := map[string]bool{}
	read := 0
	// check reads one transaction; more is false once the budget is spent.
	check := func(sig string) (vault string, more bool, err error) {
		if seen[sig] {
			return "", true, nil
		}
		if read >= budget {
			return "", false, nil
		}
		seen[sig] = true
		read++
		var tx *rpcTx
		params := []any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": 0, "commitment": "confirmed"}}
		if err := s.rpc.call(ctx, "getTransaction", params, &tx); err != nil {
			return "", false, err
		}
		if progress != nil {
			progress(read)
		}
		if tx == nil {
			return "", true, nil
		}
		return vaultOf(tx, s.cfg.Address), true, nil
	}
	for _, sig := range try {
		if v, more, err := check(sig); v != "" || !more || err != nil {
			return v, err
		}
	}
	before := ""
	for {
		opts := map[string]any{"limit": sigPageLimit, "commitment": "confirmed"}
		if before != "" {
			opts["before"] = before
		}
		var page []sigEntry
		if err := s.rpc.call(ctx, "getSignaturesForAddress", []any{s.cfg.Address, opts}, &page); err != nil {
			return "", err
		}
		for _, e := range page {
			before = e.Signature
			if !isNullJSON(e.Err) {
				continue // a failed transaction moved nothing, so it opened no vault
			}
			if v, more, err := check(e.Signature); v != "" || !more || err != nil {
				return v, err
			}
		}
		if len(page) < sigPageLimit {
			return "", nil
		}
	}
}

// vaultOf is the Jupiter limit-order vault a transaction of wallet's shows: the one other
// signer of a deposit Jupiter co-signed (the wallet pays), or of a fill or withdrawal
// Jupiter paid for. Anything else, co-signed or not, shows none.
func vaultOf(tx *rpcTx, wallet string) string {
	keys := tx.Transaction.Message.AccountKeys
	if len(keys) == 0 {
		return ""
	}
	payer := keys[0].Pubkey
	jupiter, inTx := false, false
	var others []string
	for _, k := range keys {
		switch {
		case k.Pubkey == wallet:
			inTx = true
		case !k.Signer:
		case k.Pubkey == jupiterTriggerSigner:
			jupiter = jupiter || payer == wallet
		case k.Pubkey == jupiterGasPayer:
			jupiter = jupiter || payer == jupiterGasPayer
		default:
			others = append(others, k.Pubkey)
		}
	}
	if !inTx || !jupiter || len(others) != 1 {
		return ""
	}
	return others[0]
}
