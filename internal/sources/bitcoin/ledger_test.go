package bitcoin

import (
	"testing"
)

// asXpub re-encodes an extended public key with xpub version bytes, the way Ledger shows
// every account's key whatever its script type.
func asXpub(t *testing.T, key string) string {
	t.Helper()
	payload, _, err := decodeExtendedKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return encodeAsXpub(payload)
}

// ledgerJSON is the shape Ledger Wallet shows under Edit account → Advanced.
func ledgerJSON(xpub, path, mode string) string {
	return `{
  "xpub": "` + xpub + `",
  "index": 0,
  "freshAddressPath": "` + path + `",
  "id": "js:2:bitcoin:` + xpub + `:` + mode + `",
  "blockHeight": 900000
}`
}

func TestParseLedgerExport(t *testing.T) {
	x := asXpub(t, bip84Zpub)
	tests := []struct {
		name, input, wantType string
	}{
		{"native segwit", ledgerJSON(x, "84'/0'/0'/0/20", "native_segwit"), "p2wpkh"},
		{"nested segwit", ledgerJSON(x, "49'/0'/0'/0/3", "segwit"), "p2sh-p2wpkh"},
		{"taproot", ledgerJSON(x, "86'/0'/0'/0/0", "taproot"), "p2tr"},
		{"legacy", ledgerJSON(x, "44'/0'/0'/0/7", ""), "p2pkh"},
		{"path only", `{"xpub": "` + x + `", "freshAddressPath": "84'/0'/0'/1/2"}`, "p2wpkh"},
		{"neither path nor mode", `{"xpub": "` + x + `"}`, ""},
		{"id string alone", "js:2:bitcoin:" + x + ":native_segwit", "p2wpkh"},
		{"surrounding space", "\n  " + ledgerJSON(x, "84'/0'/0'/0/1", "native_segwit") + "\n", "p2wpkh"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			key, st, ok := ParseLedgerExport(tc.input)
			if !ok || key != x || st != tc.wantType {
				t.Fatalf("= (%.12s…, %q, %v), want (%.12s…, %q, true)", key, st, ok, x, tc.wantType)
			}
		})
	}
}

func TestParseLedgerExportIgnoresOtherInput(t *testing.T) {
	for name, input := range map[string]string{
		"plain xpub":   asXpub(t, bip84Zpub),
		"descriptor":   "wpkh(" + bip84Zpub + "/<0;1>/*)",
		"address":      "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu",
		"caravan json": `{"quorum": {"requiredSigners": 2, "totalSigners": 3}, "extendedPublicKeys": []}`,
		"broken json":  `{"xpub": `,
		"empty xpub":   `{"xpub": "", "freshAddressPath": "84'/0'/0'/0/1"}`,
		"other id":     "js:2:ethereum:0xabc:",
	} {
		if key, st, ok := ParseLedgerExport(input); ok {
			t.Errorf("%s: = (%q, %q, true), want not a Ledger export", name, key, st)
		}
	}
}

// TestLedgerExportAsWalletFile: Ledger's account JSON saved to a file and uploaded scans the
// account's own script type.
func TestLedgerExportAsWalletFile(t *testing.T) {
	file := ledgerJSON(asXpub(t, bip84Zpub), "84'/0'/0'/0/20", "native_segwit")
	s := newTestSource(t, Config{WalletFile: []byte(file)})
	if a, _ := s.addressAt(0, 0); a != "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu" {
		t.Fatalf("receive 0 = %s, want the native segwit address", a)
	}
	if s.script != "Native SegWit (p2wpkh)" {
		t.Fatalf("script = %q", s.script)
	}
}
