package server

import (
	"context"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/sources"
)

const (
	vaultWallet = "4vJ9JU1bJJE96FWSJKvHsmmFADCg4gpZQff4P3bkLKi"
	testVault   = "8uvia8bNfEHFaxcEpg5uLJoTXJoZ9frsfgBU6JemUgNt"
)

// fakeVaultFinder is a Solana source whose vault is testVault for vaultWallet and none for
// any other address; it remembers the hints it was given.
type fakeVaultFinder struct {
	fakeExchange
	addr string
}

var (
	vaultHintsMu  sync.Mutex
	vaultHintsGot []string
)

func (f fakeVaultFinder) FindJupiterVault(_ context.Context, try []string, _ int, progress func(int)) (string, error) {
	vaultHintsMu.Lock()
	vaultHintsGot = try
	vaultHintsMu.Unlock()
	progress(20)
	if f.addr == vaultWallet {
		return testVault, nil
	}
	return "", nil
}

// TestFindVault: the vault field's Find button fills in the vault it finds, says when it
// finds none, and tries the edited wallet's transfers first.
func TestFindVault(t *testing.T) {
	e := newEnv(t, "")
	if _, page := e.do("GET", "/sources/new/solana", nil, nil); !strings.Contains(page, "/sources/find-vault") || !strings.Contains(page, `id="vault-find"`) {
		t.Fatal("the Solana form has no Find button for the vault")
	}

	find := func(v url.Values) string {
		body, hdr := form(v)
		_, out := e.do("POST", "/sources/find-vault", body, hdr)
		return out
	}
	if out := find(url.Values{"address": {""}}); !strings.Contains(out, "Enter the wallet") {
		t.Errorf("no address: %.400s", out)
	}
	out := find(url.Values{"address": {vaultWallet}})
	if !strings.Contains(out, `id="f-jupiterVault"`) || !strings.Contains(out, `value="`+testVault+`"`) || !strings.Contains(out, "Found") || !strings.Contains(out, "20 read") {
		t.Errorf("found: %.900s", out)
	}
	if out := find(url.Values{"address": {"7xKXtg2CW87d97TXJSDpbD5jBkheTqA83TZRuJosgAsU"}}); !strings.Contains(out, "No Jupiter limit order") || strings.Contains(out, `id="f-jupiterVault"`) {
		t.Errorf("none: %.600s", out)
	}

	e.reg.Upsert(sources.Def{ID: "sol", Type: sources.TypeSolana, Label: "Sol", Address: vaultWallet})
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	var txs []core.Tx
	for i, c := range []struct{ sig, cp string }{{"sigA", "cpX"}, {"sigB", "cpX"}, {"sigC", "cpY"}, {"sigD", ""}} {
		txs = append(txs, core.Tx{ID: "sol:" + c.sig, SourceID: "sol", ExternalID: c.sig, Time: at.Add(-time.Duration(i) * time.Hour),
			Kind: core.TxSend, Status: core.TxConfirmed, Counterparty: c.cp, Legs: []core.Leg{{AssetKey: "SOL", Symbol: "SOL", Amount: -1}}})
	}
	if _, err := e.act.Upsert(context.Background(), txs); err != nil {
		t.Fatal(err)
	}
	find(url.Values{"address": {vaultWallet}, "editId": {"sol"}})
	vaultHintsMu.Lock()
	defer vaultHintsMu.Unlock()
	if strings.Join(vaultHintsGot, ",") != "sigA,sigC" {
		t.Errorf("hints %v, want the newest transfer per counterparty: sigA, sigC", vaultHintsGot)
	}
}
