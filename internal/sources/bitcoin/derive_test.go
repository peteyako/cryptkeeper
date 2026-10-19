package bitcoin

import (
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/chaincfg/chainhash"
)

// Test vectors. Every BIP84/BIP86 string below is copied from the BIP text
// (github.com/bitcoin/bips, bip-0084.mediawiki and bip-0086.mediawiki).
// BIP44 and BIP49 contain no mainnet vectors, so those xpubs and addresses
// are cross-checked in TestMnemonicDerivesSameKeys by deriving them from
// the mnemonic with private-key derivation.
const (
	abandonMnemonic = "abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about"

	bip44Xpub = "xpub6BosfCnifzxcFwrSzQiqu2DBVTshkCXacvNsWGYJVVhhawA7d4R5WSWGFNbi8Aw6ZRc1brxMyWMzG3DSSSSoekkudhUd9yLb6qx39T9nMdj"
	bip49Ypub = "ypub6Ww3ibxVfGzLrAH1PNcjyAWenMTbbAosGNB6VvmSEgytSER9azLDWCxoJwW7Ke7icmizBMXrzBx9979FfaHxHcrArf3zbeJJJUZPf663zsP"
	bip84Zpub = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
	bip86Xpub = "xpub6BgBgsespWvERF3LHQu6CnqdvfEvtMcQjYrcRzx53QJjSxarj2afYWcLteoGVky7D3UKDP9QyrLprQ3VCECoY49yfdDEHGCtMMj92pReUsQ"

	// BIP49 text, testnet account 0 (the only BIP49 vector).
	bip49Upub = "upub5EFU65HtV5TeiSHmZZm7FUffBGy8UKeqp7vw43jYbvZPpoVsgU93oac7Wk3u6moKegAEWtGNF8DehrnHtv21XXEMYRUocHqguyjknFHYfgY"
	// BIP86 text, account 0 xprv (a private key: must be refused).
	bip86Xprv = "xprv9xgqHN7yz9MwCkxsBPN5qetuNdQSUttZNKw1dcYTV4mkaAFiBVGQziHs3NRSWMkCzvgjEe3n9xV8oYywvM8at9yRqyaZVz6TYYhX98VjsUk"
	// BIP84 text, account 0 zprv.
	bip84Zprv = "zprvAdG4iTXWBoARxkkzNpNh8r6Qag3irQB8PzEMkAFeTRXxHpbF9z4QgEvBRmfvqWvGp42t42nvgGpNgYSJA9iefm1yYNZKEm7z6qUWCroSQnE"
	// BIP380 text, WIF compressed private key.
	wifKey = "L4rK1yDtCWekvXuE6oXD9jCYfFNV2cWRpVuPLBcCU2z8TrisoyY1"
)

func seedFromMnemonic(t *testing.T, mnemonic string) []byte {
	t.Helper()
	seed, err := pbkdf2.Key(sha512.New, mnemonic, []byte("mnemonic"), 2048, 64)
	if err != nil {
		t.Fatal(err)
	}
	return seed
}

func derivePath(t *testing.T, k *hdkeychain.ExtendedKey, path ...uint32) *hdkeychain.ExtendedKey {
	t.Helper()
	for _, p := range path {
		var err error
		k, err = k.Derive(p)
		if err != nil {
			t.Fatal(err)
		}
	}
	return k
}

const h = hdkeychain.HardenedKeyStart

// withVersion re-encodes an extended key with different version bytes (test helper).
func withVersion(t *testing.T, key string, version []byte) string {
	t.Helper()
	raw := base58.Decode(key)
	if len(raw) != 82 {
		t.Fatalf("decode: bad length %d", len(raw))
	}
	payload := append([]byte(nil), raw[:78]...)
	copy(payload[:4], version)
	return base58.Encode(append(payload, chainhash.DoubleHashB(payload)[:4]...))
}

func newTestSource(t *testing.T, cfg Config) *Source {
	t.Helper()
	if cfg.EsploraURL == "" {
		cfg.EsploraURL = "http://esplora.invalid/api"
	}
	s, err := New(cfg, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestExtendedKeyFirstAddresses(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		scriptType string
		chain, idx int
		want       string
	}{
		{"BIP44 xpub receive 0", bip44Xpub, "", 0, 0, "1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA"},
		{"BIP49 ypub receive 0", bip49Ypub, "", 0, 0, "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf"},
		{"BIP84 zpub receive 0", bip84Zpub, "", 0, 0, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"},
		{"BIP84 zpub receive 1", bip84Zpub, "", 0, 1, "bc1qnjg0jd8228aq7egyzacy8cys3knf9xvrerkf9g"},
		{"BIP84 zpub change 0", bip84Zpub, "", 1, 0, "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el"},
		{"BIP86 xpub p2tr receive 0", bip86Xpub, "p2tr", 0, 0, "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr"},
		{"BIP86 xpub p2tr receive 1", bip86Xpub, "p2tr", 0, 1, "bc1p4qhjn9zdvkux4e44uhx8tc55attvtyu358kutcqkudyccelu0was9fqzwh"},
		{"BIP86 xpub p2tr change 0", bip86Xpub, "p2tr", 1, 0, "bc1p3qkhfews2uk44qtvauqyr2ttdsw7svhkl9nkm9s9c3x4ax5h60wqwruhk7"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSource(t, Config{ID: "w", Key: tc.key, ScriptType: tc.scriptType})
			got, err := s.addressAt(tc.chain, tc.idx)
			if err != nil {
				t.Fatalf("addressAt: %v", err)
			}
			if got != tc.want {
				t.Fatalf("address %d/%d = %s, want %s", tc.chain, tc.idx, got, tc.want)
			}
		})
	}
}

func TestScriptTypeOverride(t *testing.T) {
	// The same BIP84 account key forced to p2pkh must give the legacy encoding of the
	// same pubkey hash as the native segwit vector.
	s := newTestSource(t, Config{Key: bip84Zpub, ScriptType: "p2pkh"})
	got, err := s.addressAt(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	seg, _ := btcutil.DecodeAddress("bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu", &chaincfg.MainNetParams)
	legacy, _ := btcutil.NewAddressPubKeyHash(seg.ScriptAddress(), &chaincfg.MainNetParams)
	if got != legacy.EncodeAddress() {
		t.Fatalf("override p2pkh = %s, want %s", got, legacy.EncodeAddress())
	}
}

// TestLedgerXpubWithScriptType: Ledger shows every account's key as an xpub, whatever its
// script type. Left on auto-detect it derives legacy addresses; with the account's script
// type set it derives the same addresses as the matching ypub/zpub.
func TestLedgerXpubWithScriptType(t *testing.T) {
	tests := []struct {
		name, key, scriptType, want string
	}{
		{"native segwit", asXpub(t, bip84Zpub), "p2wpkh", "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"},
		{"nested segwit", asXpub(t, bip49Ypub), "p2sh-p2wpkh", "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			auto, err := newTestSource(t, Config{Key: tc.key}).addressAt(0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(auto, "1") {
				t.Errorf("auto-detect = %s, want a legacy 1… address", auto)
			}
			got, err := newTestSource(t, Config{Key: tc.key, ScriptType: tc.scriptType}).addressAt(0, 0)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("%s = %s, want %s", tc.scriptType, got, tc.want)
			}
		})
	}
}

func TestInvalidScriptTypeRejected(t *testing.T) {
	_, err := New(Config{Key: bip84Zpub, ScriptType: "p2wsh-magic", EsploraURL: "http://x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "script type") {
		t.Fatalf("want script type error, got %v", err)
	}
}

func TestMnemonicDerivesSameKeys(t *testing.T) {
	seed := seedFromMnemonic(t, abandonMnemonic)
	master, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		purpose uint32
		version []byte
		want    string
	}{
		{44, nil, bip44Xpub},
		{49, []byte{0x04, 0x9d, 0x7c, 0xb2}, bip49Ypub},
		{84, []byte{0x04, 0xb2, 0x47, 0x46}, bip84Zpub},
		{86, nil, bip86Xpub},
	}
	for _, c := range cases {
		acct := derivePath(t, master, h+c.purpose, h+0, h+0)
		pub, err := acct.Neuter()
		if err != nil {
			t.Fatal(err)
		}
		got := pub.String()
		if c.version != nil {
			got = withVersion(t, got, c.version)
		}
		if got != c.want {
			t.Errorf("m/%d'/0'/0' = %s, want %s", c.purpose, got, c.want)
		}
	}

	// Private derivation of m/44'/0'/0'/0/0 and m/49'/0'/0'/0/0 must give the same
	// addresses as the public derivation the source does.
	priv44 := derivePath(t, master, h+44, h+0, h+0, 0, 0)
	pk44, _ := priv44.ECPubKey()
	a44, _ := btcutil.NewAddressPubKeyHash(btcutil.Hash160(pk44.SerializeCompressed()), &chaincfg.MainNetParams)
	if a44.EncodeAddress() != "1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA" {
		t.Errorf("BIP44 m/44'/0'/0'/0/0 = %s", a44.EncodeAddress())
	}
	priv49 := derivePath(t, master, h+49, h+0, h+0, 0, 0)
	pk49, _ := priv49.ECPubKey()
	redeem := append([]byte{0x00, 0x14}, btcutil.Hash160(pk49.SerializeCompressed())...)
	a49, _ := btcutil.NewAddressScriptHash(redeem, &chaincfg.MainNetParams)
	if a49.EncodeAddress() != "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf" {
		t.Errorf("BIP49 m/49'/0'/0'/0/0 = %s", a49.EncodeAddress())
	}
}

// TestBIP49TestnetVector checks the nested-segwit construction against the only
// vector in the BIP49 text (testnet, m/49'/1'/0'/0/0).
func TestBIP49TestnetVector(t *testing.T) {
	seed := seedFromMnemonic(t, abandonMnemonic)
	master, err := hdkeychain.NewMaster(seed, &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}
	acct, _ := derivePath(t, master, h+49, h+1, h+0).Neuter()
	if got := withVersion(t, acct.String(), []byte{0x04, 0x4a, 0x52, 0x62}); got != bip49Upub {
		t.Fatalf("account0Xpub = %s, want %s", got, bip49Upub)
	}
	child := derivePath(t, acct, 0, 0)
	pub, _ := child.ECPubKey()
	if got := hex.EncodeToString(pub.SerializeCompressed()); got != "03a1af804ac108a8a51782198c2d034b28bf90c8803f5a53f76276fa69a4eae77f" {
		t.Fatalf("account0recvPublicKeyHex = %s", got)
	}
	addr, err := addressForKey(scriptP2SHP2WPKH, pub.SerializeCompressed(), pub, &chaincfg.TestNet3Params)
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(addr.ScriptAddress()); got != "336caa13e08b96080a32b5d818d59b4ab3b36742" {
		t.Errorf("addressBytes = %s", got)
	}
	if addr.EncodeAddress() != "2Mww8dCYPUpKHofjgcXcBCEGmniw9CoaiD2" {
		t.Errorf("address = %s", addr.EncodeAddress())
	}
}

func TestTestnetKeysRejected(t *testing.T) {
	seed := seedFromMnemonic(t, abandonMnemonic)
	master, _ := hdkeychain.NewMaster(seed, &chaincfg.TestNet3Params)
	tpubKey, _ := derivePath(t, master, h+84, h+1, h+0).Neuter()
	tpub := tpubKey.String()
	vpub := withVersion(t, tpub, []byte{0x04, 0x5f, 0x1c, 0xf6})
	for name, key := range map[string]string{"tpub": tpub, "upub": bip49Upub, "vpub": vpub} {
		_, err := New(Config{Key: key, EsploraURL: "http://x"}, nil)
		if err == nil || !strings.Contains(err.Error(), "mainnet only") {
			t.Errorf("%s: want mainnet only error, got %v", name, err)
		}
	}
}

func TestPrivateKeysRefused(t *testing.T) {
	for name, key := range map[string]string{"xprv": bip86Xprv, "zprv": bip84Zprv, "wif": wifKey} {
		_, err := New(Config{Key: key, EsploraURL: "http://x"}, nil)
		if err == nil || !strings.Contains(err.Error(), "private key") {
			t.Errorf("%s: want private key refusal, got %v", name, err)
		}
		if err != nil && strings.Contains(err.Error(), key) {
			t.Errorf("%s: error echoes the key: %v", name, err)
		}
	}
}

func TestBadKeyErrorDoesNotLeakKey(t *testing.T) {
	// Flip one character so the base58 checksum fails.
	bad := bip84Zpub[:len(bip84Zpub)-1] + "t"
	_, err := New(Config{Key: bad, EsploraURL: "http://x"}, nil)
	if err == nil {
		t.Fatal("want error for corrupted key")
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("error leaks full key: %v", err)
	}
}

func TestCosignerKeyAloneRejected(t *testing.T) {
	zpubMultisig := withVersion(t, bip44Xpub, []byte{0x02, 0xaa, 0x7e, 0xd3}) // Zpub
	_, err := New(Config{Key: zpubMultisig, EsploraURL: "http://x"}, nil)
	if err == nil || !strings.Contains(err.Error(), "multisig") {
		t.Fatalf("want multisig cosigner error, got %v", err)
	}
}

// TestDerivedAddresses: the first n receive then change addresses, with no server; a single
// address lists itself once.
func TestDerivedAddresses(t *testing.T) {
	got := newTestSource(t, Config{ID: "w", Key: bip86Xpub, ScriptType: "p2tr"}).DerivedAddresses(2)
	want := []string{
		"bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr",
		"bc1p4qhjn9zdvkux4e44uhx8tc55attvtyu358kutcqkudyccelu0was9fqzwh",
		"bc1p3qkhfews2uk44qtvauqyr2ttdsw7svhkl9nkm9s9c3x4ax5h60wqwruhk7",
	}
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("derived %v", got)
	}
	single := newTestSource(t, Config{ID: "a", Key: "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"}).DerivedAddresses(5)
	if len(single) != 1 || single[0] != "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu" {
		t.Fatalf("single address %v", single)
	}
}
