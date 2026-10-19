package bitcoin

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// DescriptorFromWalletFile extracts an output descriptor from an uploaded wallet file:
//   - Caravan/Unchained multisig JSON (quorum + extendedPublicKeys) becomes a checksummed
//     sortedmulti descriptor with /<0;1>/* derivation (Caravan uses BIP67 sorted multisig);
//   - JSON with a "descriptor" field (Specter, BlueWallet and similar exports);
//   - Ledger Wallet's account export (the Advanced JSON with "xpub" and "freshAddressPath");
//   - a text file (Sparrow "Output Descriptor" export): the first line that parses as a
//     descriptor. Lines starting with '#' are comments. If that line is a receive-only /0/*
//     descriptor and the next descriptor is its /1/* change twin, both are returned, one per line.
func DescriptorFromWalletFile(data []byte) (string, error) {
	text := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if text == "" {
		return "", errors.New("wallet file is empty")
	}
	if strings.HasPrefix(text, "{") {
		return fromJSONWalletFile([]byte(text))
	}
	return fromTextWalletFile(text)
}

type caravanKey struct {
	Name      string `json:"name"`
	XPub      string `json:"xpub"`
	BIP32Path string `json:"bip32Path"`
	XFP       string `json:"xfp"`
}

type jsonWalletFile struct {
	AddressType string `json:"addressType"`
	Network     string `json:"network"`
	Quorum      *struct {
		RequiredSigners int `json:"requiredSigners"`
		TotalSigners    int `json:"totalSigners"`
	} `json:"quorum"`
	ExtendedPublicKeys []caravanKey `json:"extendedPublicKeys"`
	Descriptor         string       `json:"descriptor"`
}

func fromJSONWalletFile(b []byte) (string, error) {
	var f jsonWalletFile
	if err := json.Unmarshal(b, &f); err != nil {
		return "", fmt.Errorf("wallet file is not valid JSON: %w", err)
	}
	switch {
	case f.Quorum != nil || len(f.ExtendedPublicKeys) > 0:
		return caravanToDescriptor(f)
	case strings.TrimSpace(f.Descriptor) != "":
		desc := strings.TrimSpace(f.Descriptor)
		if _, err := chainsFromDescriptorText(desc); err != nil {
			return "", fmt.Errorf("wallet file descriptor: %w", err)
		}
		return desc, nil
	}
	if key, st, ok := ParseLedgerExport(string(b)); ok {
		return ledgerDescriptor(key, st)
	}
	return "", errors.New(`unrecognised JSON wallet file: expected a Caravan/Unchained multisig config, a Ledger account export or a "descriptor" field`)
}

func caravanToDescriptor(f jsonWalletFile) (string, error) {
	switch strings.ToLower(strings.TrimSpace(f.Network)) {
	case "", "mainnet", "main", "bitcoin":
	default:
		return "", errTestnet("wallet file network " + strconv.Quote(f.Network))
	}
	n := len(f.ExtendedPublicKeys)
	if n == 0 {
		return "", errors.New("Caravan wallet file has no extendedPublicKeys")
	}
	m := 0
	if f.Quorum != nil {
		m = f.Quorum.RequiredSigners
		if f.Quorum.TotalSigners != 0 && f.Quorum.TotalSigners != n {
			return "", fmt.Errorf("Caravan wallet file: totalSigners %d does not match its %d extendedPublicKeys", f.Quorum.TotalSigners, n)
		}
	}
	if m < 1 || m > n {
		return "", fmt.Errorf("Caravan wallet file: requiredSigners %d must be between 1 and the number of keys (%d)", m, n)
	}
	var wrap string
	switch at := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(f.AddressType), "_", "-")); at {
	case "P2WSH":
		wrap = "wsh(%s)"
	case "P2SH-P2WSH":
		wrap = "sh(wsh(%s))"
	case "P2SH":
		wrap = "sh(%s)"
	default:
		return "", fmt.Errorf("unsupported Caravan address type %q (want P2WSH, P2SH-P2WSH or P2SH)", f.AddressType)
	}
	keys := make([]string, n)
	for i, k := range f.ExtendedPublicKeys {
		ks, err := caravanKeyExpr(k)
		if err != nil {
			return "", fmt.Errorf("Caravan key #%d (%s): %w", i+1, k.Name, err)
		}
		keys[i] = ks
	}
	body := fmt.Sprintf(wrap, fmt.Sprintf("sortedmulti(%d,%s)", m, strings.Join(keys, ",")))
	sum, err := descChecksum(body)
	if err != nil {
		return "", err
	}
	desc := body + "#" + sum
	if _, err := chainsFromDescriptorText(desc); err != nil {
		return "", fmt.Errorf("Caravan wallet file: %w", err)
	}
	return desc, nil
}

// caravanKeyExpr renders one cosigner as [xfp/path]xpub/<0;1>/*. SLIP-132 versions
// (Zpub, Ypub…) are rewritten to xpub; an unknown xfp drops the origin, an unknown path
// keeps just the fingerprint.
func caravanKeyExpr(k caravanKey) (string, error) {
	payload, v, err := decodeExtendedKey(strings.TrimSpace(k.XPub))
	if err != nil {
		return "", err
	}
	if v.private {
		return "", errPrivateKey
	}
	if v.testnet {
		return "", errTestnet(v.name)
	}
	origin := ""
	if xfp := strings.ToLower(strings.TrimSpace(k.XFP)); len(xfp) == 8 && isHex(xfp) {
		origin = "[" + xfp + caravanPath(k.BIP32Path) + "]"
	}
	return origin + encodeAsXpub(payload) + "/<0;1>/*", nil
}

// caravanPath converts "m/48'/0'/0'/2'" to "/48h/0h/0h/2h"; anything unparseable
// (Caravan writes "Unknown") becomes "".
func caravanPath(p string) string {
	p = strings.TrimSpace(p)
	rest, ok := strings.CutPrefix(p, "m/")
	if !ok {
		if rest, ok = strings.CutPrefix(p, "M/"); !ok {
			return ""
		}
	}
	var b strings.Builder
	for _, el := range strings.Split(rest, "/") {
		idx, hardened, err := parseIndex(el)
		if err != nil {
			return ""
		}
		b.WriteString("/" + strconv.FormatUint(uint64(idx), 10))
		if hardened {
			b.WriteString("h")
		}
	}
	return b.String()
}

func fromTextWalletFile(text string) (string, error) {
	var found []string
	var parsed []*descriptor
	var firstErr error
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d, err := parseDescriptor(line)
		if err != nil {
			if firstErr == nil && strings.Contains(line, "(") {
				firstErr = err
			}
			continue
		}
		found, parsed = append(found, line), append(parsed, d)
	}
	if len(found) == 0 {
		if firstErr != nil {
			return "", fmt.Errorf("no output descriptor found in the wallet file (first candidate: %w)", firstErr)
		}
		return "", errors.New("no output descriptor found in the wallet file")
	}
	if len(found) >= 2 && parsed[0].ranged() && parsed[0].npaths == 1 && isChangeTwin(found[0], found[1]) {
		return found[0] + "\n" + found[1], nil
	}
	return found[0], nil
}

// isChangeTwin reports whether change is receive with its /0/* steps replaced by /1/*.
func isChangeTwin(receive, change string) bool {
	r, _, _ := strings.Cut(receive, "#")
	c, _, _ := strings.Cut(change, "#")
	return strings.Contains(r, "/0/*") && strings.ReplaceAll(r, "/0/*", "/1/*") == c
}
