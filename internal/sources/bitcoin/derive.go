package bitcoin

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
	"github.com/btcsuite/btcd/txscript"
)

// mainnet is the only network CryptKeeper supports.
var mainnet = &chaincfg.MainNetParams

// scriptType is the output type of a single-key HD account.
type scriptType int

const (
	scriptP2PKH scriptType = iota + 1
	scriptP2SHP2WPKH
	scriptP2WPKH
	scriptP2TR
)

func parseScriptType(s string) (scriptType, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "p2pkh":
		return scriptP2PKH, nil
	case "p2sh-p2wpkh":
		return scriptP2SHP2WPKH, nil
	case "p2wpkh":
		return scriptP2WPKH, nil
	case "p2tr":
		return scriptP2TR, nil
	}
	return 0, fmt.Errorf("unknown script type %q (want p2pkh, p2sh-p2wpkh, p2wpkh or p2tr)", s)
}

// label is the human name shown in the wallet's Connection panel.
func (st scriptType) label() string {
	switch st {
	case scriptP2PKH:
		return "Legacy (p2pkh)"
	case scriptP2SHP2WPKH:
		return "Nested SegWit (p2sh-p2wpkh)"
	case scriptP2WPKH:
		return "Native SegWit (p2wpkh)"
	case scriptP2TR:
		return "Taproot (p2tr)"
	}
	return "unknown"
}

// bip43Purpose is the standard BIP43 purpose for a single-key script type.
func (st scriptType) bip43Purpose() uint32 {
	switch st {
	case scriptP2PKH:
		return 44
	case scriptP2SHP2WPKH:
		return 49
	case scriptP2WPKH:
		return 84
	case scriptP2TR:
		return 86
	}
	return 0
}

// keyVersion describes the 4 version bytes of a serialized extended key (BIP32 and SLIP-132).
type keyVersion struct {
	name     string
	private  bool
	testnet  bool
	multisig bool       // Ypub/Zpub style cosigner keys
	script   scriptType // default script for single-key versions
}

var keyVersions = map[uint32]keyVersion{
	0x0488b21e: {name: "xpub", script: scriptP2PKH},
	0x049d7cb2: {name: "ypub", script: scriptP2SHP2WPKH},
	0x04b24746: {name: "zpub", script: scriptP2WPKH},
	0x0295b43f: {name: "Ypub", multisig: true},
	0x02aa7ed3: {name: "Zpub", multisig: true},

	0x0488ade4: {name: "xprv", private: true},
	0x049d7878: {name: "yprv", private: true},
	0x04b2430c: {name: "zprv", private: true},
	0x0295b005: {name: "Yprv", private: true},
	0x02aa7a99: {name: "Zprv", private: true},

	0x043587cf: {name: "tpub", testnet: true},
	0x044a5262: {name: "upub", testnet: true},
	0x045f1cf6: {name: "vpub", testnet: true},
	0x024289ef: {name: "Upub", testnet: true},
	0x02575483: {name: "Vpub", testnet: true},

	0x04358394: {name: "tprv", private: true, testnet: true},
	0x044a4e28: {name: "uprv", private: true, testnet: true},
	0x045f18bc: {name: "vprv", private: true, testnet: true},
	0x024285b5: {name: "Uprv", private: true, testnet: true},
	0x02575048: {name: "Vprv", private: true, testnet: true},
}

var xpubVersion = []byte{0x04, 0x88, 0xb2, 0x1e}

var errPrivateKey = errors.New("that is a private key: CryptKeeper is read-only and only accepts public keys " +
	"(xpub/ypub/zpub), descriptors with public keys, or addresses. Never paste private keys or seed phrases")

func errTestnet(what string) error {
	return fmt.Errorf("%s is a testnet key or address: CryptKeeper is mainnet only", what)
}

// extendedKeyPrefixes are the leading characters of every version in keyVersions.
var extendedKeyPrefixes = []string{
	"xpub", "ypub", "zpub", "Ypub", "Zpub", "xprv", "yprv", "zprv", "Yprv", "Zprv",
	"tpub", "upub", "vpub", "Upub", "Vpub", "tprv", "uprv", "vprv", "Uprv", "Vprv",
}

// looksLikeExtendedKey reports whether s is shaped like a serialized extended key.
func looksLikeExtendedKey(s string) bool {
	for _, p := range extendedKeyPrefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return len(base58.Decode(s)) == 82
}

// redact shortens key material for error messages, so full xpubs never reach logs.
func redact(s string) string {
	if len(s) <= 16 {
		return s
	}
	return s[:8] + "…" + s[len(s)-4:]
}

// decodeExtendedKey base58check-decodes s and returns its 78-byte payload and version info.
func decodeExtendedKey(s string) ([]byte, keyVersion, error) {
	raw := base58.Decode(s)
	if len(raw) != 82 {
		return nil, keyVersion{}, fmt.Errorf("extended key %s has the wrong length (truncated or mistyped?)", redact(s))
	}
	payload, sum := raw[:78], raw[78:]
	if string(chainhash.DoubleHashB(payload)[:4]) != string(sum) {
		return nil, keyVersion{}, fmt.Errorf("extended key %s fails its checksum (mistyped?)", redact(s))
	}
	v, ok := keyVersions[binary.BigEndian.Uint32(payload[:4])]
	if !ok {
		return nil, keyVersion{}, fmt.Errorf("extended key %s has unknown version bytes %x", redact(s), payload[:4])
	}
	return payload, v, nil
}

// parseExtendedPub parses a mainnet extended public key of any SLIP-132 version by rewriting
// its version to xpub. Private and testnet keys are refused.
func parseExtendedPub(s string) (*hdkeychain.ExtendedKey, keyVersion, error) {
	payload, v, err := decodeExtendedKey(s)
	if err != nil {
		return nil, v, err
	}
	if v.private {
		return nil, v, errPrivateKey
	}
	if v.testnet {
		return nil, v, errTestnet(v.name)
	}
	k, err := hdkeychain.NewKeyFromString(encodeAsXpub(payload))
	if err != nil {
		return nil, v, fmt.Errorf("extended key %s is invalid: %w", redact(s), err)
	}
	return k, v, nil
}

// encodeAsXpub re-encodes a 78-byte extended public key payload with xpub version bytes.
func encodeAsXpub(payload []byte) string {
	b := make([]byte, 0, 82)
	b = append(b, xpubVersion...)
	b = append(b, payload[4:78]...)
	b = append(b, chainhash.DoubleHashB(b)[:4]...)
	return base58.Encode(b)
}

// addressForKey returns the address of a public key for a single-key script type. ser is the
// key's serialization: compressed, except that legacy p2pkh may use an uncompressed key.
func addressForKey(st scriptType, ser []byte, pub *btcec.PublicKey, net *chaincfg.Params) (btcutil.Address, error) {
	switch st {
	case scriptP2PKH:
		return btcutil.NewAddressPubKeyHash(btcutil.Hash160(ser), net)
	case scriptP2WPKH:
		return btcutil.NewAddressWitnessPubKeyHash(btcutil.Hash160(ser), net)
	case scriptP2SHP2WPKH:
		redeem := append([]byte{txscript.OP_0, txscript.OP_DATA_20}, btcutil.Hash160(ser)...)
		return btcutil.NewAddressScriptHash(redeem, net)
	case scriptP2TR:
		out := txscript.ComputeTaprootKeyNoScript(pub)
		return btcutil.NewAddressTaproot(schnorr.SerializePubKey(out), net)
	}
	return nil, fmt.Errorf("unsupported script type %d", st)
}
