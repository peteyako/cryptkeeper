package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
)

// signedTx is a getTransaction result whose account keys are keys, the first signers of
// them signing (the first one pays the fee).
func signedTx(signers int, keys ...string) json.RawMessage {
	type key struct {
		Pubkey   string `json:"pubkey"`
		Signer   bool   `json:"signer"`
		Writable bool   `json:"writable"`
	}
	ks := make([]key, len(keys))
	for i, k := range keys {
		ks[i] = key{Pubkey: k, Signer: i < signers, Writable: true}
	}
	raw, _ := json.Marshal(map[string]any{
		"slot": 1, "blockTime": 1767225600,
		"transaction": map[string]any{"message": map[string]any{"accountKeys": ks, "instructions": []any{}}},
		"meta":        map[string]any{"err": nil, "fee": 5000, "preBalances": []any{}, "postBalances": []any{}},
	})
	return raw
}

// TestFindJupiterVault: the vault is the other signer of a deposit Jupiter co-signed, or of a
// fill or withdrawal Jupiter paid for; a transaction someone else co-signed is not one.
func TestFindJupiterVault(t *testing.T) {
	vault, friend, ata := testMint("vault"), testMint("friend"), testMint("vault-ata")
	for _, tc := range []struct {
		name string
		tx   func(wallet string) json.RawMessage
		want string
	}{
		{"deposit", func(w string) json.RawMessage { return signedTx(3, w, vault, jupiterTriggerSigner, ata) }, vault},
		{"withdrawal", func(w string) json.RawMessage { return signedTx(2, jupiterGasPayer, vault, w, ata) }, vault},
		{"co-signed by someone else", func(w string) json.RawMessage { return signedTx(2, w, friend, ata) }, ""},
		{"Jupiter with two others", func(w string) json.RawMessage { return signedTx(4, w, vault, friend, jupiterTriggerSigner) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l, s := newLedger(t, testOwner)
			l.addRaw("old", 1767225600, nil, []string{testOwner}, tc.tx(testOwner))
			l.addRaw("new", 1767225700, nil, []string{testOwner}, signedTx(1, testOwner, friend))
			got, err := s.FindJupiterVault(context.Background(), nil, 50, nil)
			if err != nil || got != tc.want {
				t.Fatalf("FindJupiterVault = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// TestFindJupiterVaultBudget: the likely signatures are read first, and no more than budget
// transactions are read in all; reading progress is reported as it goes.
func TestFindJupiterVaultBudget(t *testing.T) {
	vault := testMint("vault")
	l, s := newLedger(t, testOwner)
	l.addRaw("deposit", 1767225600, nil, []string{testOwner}, signedTx(3, testOwner, vault, jupiterTriggerSigner))
	for i := range 5 {
		l.addRaw(fmt.Sprintf("noise%d", i), int64(1767225700+i), nil, []string{testOwner}, signedTx(1, testOwner, testMint("x")))
	}
	var read []int
	got, err := s.FindJupiterVault(context.Background(), nil, 3, func(n int) { read = append(read, n) })
	if err != nil || got != "" || l.callCount("getTransaction") != 3 {
		t.Fatalf("budget 3: %q, %v after %d transactions; want nothing after 3", got, err, l.callCount("getTransaction"))
	}
	if len(read) != 3 || read[2] != 3 {
		t.Errorf("progress %v, want 1, 2, 3", read)
	}

	l2, s2 := newLedger(t, testOwner)
	l2.addRaw("deposit", 1767225600, nil, []string{testOwner}, signedTx(3, testOwner, vault, jupiterTriggerSigner))
	for i := range 5 {
		l2.addRaw(fmt.Sprintf("noise%d", i), int64(1767225700+i), nil, []string{testOwner}, signedTx(1, testOwner, testMint("x")))
	}
	got, err = s2.FindJupiterVault(context.Background(), []string{"deposit"}, 3, nil)
	if err != nil || got != vault || l2.callCount("getTransaction") != 1 || l2.callCount("getSignaturesForAddress") != 0 {
		t.Fatalf("with the deposit as a hint: %q, %v after %d transactions, %d lists; want it at once",
			got, err, l2.callCount("getTransaction"), l2.callCount("getSignaturesForAddress"))
	}
}
