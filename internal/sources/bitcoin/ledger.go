package bitcoin

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ledgerAccount is what Ledger Wallet (formerly Ledger Live) shows for a Bitcoin account
// under Edit account → Advanced. Its "id" ends in the account's derivation mode.
type ledgerAccount struct {
	XPub             string `json:"xpub"`
	FreshAddressPath string `json:"freshAddressPath"`
	ID               string `json:"id"`
}

// ledgerModes maps Ledger's derivation modes to script types ("" is Legacy).
var ledgerModes = map[string]string{
	"":              "p2pkh",
	"segwit":        "p2sh-p2wpkh",
	"native_segwit": "p2wpkh",
	"taproot":       "p2tr",
}

// purposeScripts maps the BIP purpose (the first step of a derivation path) to a script type.
var purposeScripts = map[string]string{"44": "p2pkh", "49": "p2sh-p2wpkh", "84": "p2wpkh", "86": "p2tr"}

// ParseLedgerExport reads a Ledger Wallet account export: the Advanced JSON, or its "id" on
// its own ("js:2:bitcoin:xpub…:native_segwit"). It returns the xpub and its script type
// ("p2wpkh", …; "" when the export doesn't say). ok is false when input isn't one. Ledger
// shows an xpub for every account, so without the script type a SegWit account would be
// read as Legacy.
func ParseLedgerExport(input string) (key, scriptType string, ok bool) {
	s := strings.TrimSpace(input)
	var a ledgerAccount
	switch {
	case strings.HasPrefix(s, "{"):
		if json.Unmarshal([]byte(s), &a) != nil || strings.TrimSpace(a.XPub) == "" {
			return "", "", false
		}
	case strings.HasPrefix(s, "js:"):
		a.ID = s
	default:
		return "", "", false
	}
	idKey, mode, hasMode := parseLedgerID(a.ID)
	key = strings.TrimSpace(a.XPub)
	if key == "" {
		key = idKey
	}
	if key == "" {
		return "", "", false
	}
	if hasMode {
		scriptType = ledgerModes[mode]
	}
	if scriptType == "" {
		purpose, _, _ := strings.Cut(a.FreshAddressPath, "/")
		scriptType = purposeScripts[strings.TrimRight(purpose, "'h")]
	}
	return key, scriptType, true
}

// parseLedgerID splits "js:2:bitcoin:<xpub>:<mode>" into the xpub and the mode.
func parseLedgerID(id string) (key, mode string, ok bool) {
	parts := strings.Split(strings.TrimSpace(id), ":")
	if len(parts) != 5 || parts[0] != "js" || !strings.HasPrefix(parts[2], "bitcoin") {
		return "", "", false
	}
	return parts[3], parts[4], true
}

// ledgerDescriptor turns a Ledger export into the account's receive/change descriptor.
func ledgerDescriptor(key, scriptType string) (string, error) {
	formats := map[string]string{
		"p2pkh":       "pkh(%s/<0;1>/*)",
		"p2sh-p2wpkh": "sh(wpkh(%s/<0;1>/*))",
		"p2wpkh":      "wpkh(%s/<0;1>/*)",
		"p2tr":        "tr(%s/<0;1>/*)",
	}
	if scriptType == "" {
		scriptType = "p2pkh"
	}
	desc := fmt.Sprintf(formats[scriptType], key)
	if _, err := chainsFromDescriptorText(desc); err != nil {
		return "", fmt.Errorf("Ledger account export: %w", err)
	}
	return desc, nil
}
