package bitcoin

import (
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
)

func TestDetect(t *testing.T) {
	cases := map[string]string{
		bip44Xpub:         "xpub",
		"  " + bip49Ypub:  "ypub",
		bip84Zpub + "\n":  "zpub",
		bip86Xpub:         "xpub",
		caravanDescriptor: "descriptor",
		"wpkh(" + bip84Zpub + "/0/*)\nwpkh(" + bip84Zpub + "/1/*)":       "descriptor",
		"addr(bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu)":               "descriptor",
		"1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA":                             "address",
		"37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf":                             "address",
		"bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu":                     "address",
		"bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr": "address",
	}
	for in, want := range cases {
		got, err := Detect(in)
		if err != nil || got != want {
			t.Errorf("Detect(%.30q) = %q, %v; want %q", in, got, err, want)
		}
	}
}

func TestDetectRejects(t *testing.T) {
	seed := seedFromMnemonic(t, abandonMnemonic)
	master, _ := hdkeychain.NewMaster(seed, &chaincfg.TestNet3Params)
	tpubKey, _ := derivePath(t, master, h+84, h+1, h+0).Neuter()
	tpub := tpubKey.String()
	vpub := withVersion(t, tpub, []byte{0x04, 0x5f, 0x1c, 0xf6})
	tb1, _ := btcutil.NewAddressWitnessPubKeyHash(make([]byte, 20), &chaincfg.TestNet3Params)

	cases := []struct{ in, want string }{
		{tpub, "mainnet only"},
		{bip49Upub, "mainnet only"},
		{vpub, "mainnet only"},
		{"2Mww8dCYPUpKHofjgcXcBCEGmniw9CoaiD2", "mainnet only"},
		{tb1.EncodeAddress(), "mainnet only"},
		{bip86Xprv, "private key"},
		{bip84Zprv, "private key"},
		{wifKey, "private key"},
		{withVersion(t, bip44Xpub, []byte{0x02, 0xaa, 0x7e, 0xd3}), "multisig"},
		{"wpkh(" + bip84Zpub + "/<0;1>/*)#aaaaaaaa", "checksum"},
		{`{"quorum":{"requiredSigners":2}}`, "wallet file"},
		{"", "empty"},
		{"hello world", "not a"},
	}
	for _, c := range cases {
		_, err := Detect(c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("Detect(%.30q): want error containing %q, got %v", c.in, c.want, err)
		}
	}
}

func TestDetectDoesNotLeakKey(t *testing.T) {
	bad := bip44Xpub[:len(bip44Xpub)-2] + "zz"
	_, err := Detect(bad)
	if err == nil {
		t.Fatal("want error for corrupted xpub")
	}
	if strings.Contains(err.Error(), bad) {
		t.Fatalf("error leaks key: %v", err)
	}
}

func TestNewValidatesConfig(t *testing.T) {
	cases := map[string]Config{
		"nothing given":       {EsploraURL: "http://x"},
		"key and descriptor":  {Key: bip84Zpub, Descriptor: caravanDescriptor},
		"key and wallet file": {Key: bip84Zpub, WalletFile: []byte("x")},
		"bad esplora scheme":  {Key: bip84Zpub, EsploraURL: "ftp://mempool.space/api"},
		"bad esplora url":     {Key: bip84Zpub, EsploraURL: "mempool.space"},
		"bad address":         {Key: "bc1qnotanaddress"},
	}
	for name, cfg := range cases {
		if _, err := New(cfg, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNewDefaults(t *testing.T) {
	s, err := New(Config{ID: "cold", Key: bip84Zpub}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.ID() != "cold" || s.gap != DefaultGapLimit || s.esplora != DefaultEsploraURL || s.node != "mempool.space" || s.client == nil {
		t.Fatalf("defaults: id %q gap %d esplora %q node %q", s.ID(), s.gap, s.esplora, s.node)
	}
	s, err = New(Config{Key: bip84Zpub, EsploraURL: "https://node.lan:3002/api/", GapLimit: 50}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.esplora != "https://node.lan:3002/api" || s.node != "node.lan:3002" || s.gap != 50 {
		t.Fatalf("esplora %q node %q gap %d", s.esplora, s.node, s.gap)
	}
}

func TestDescriptorInKeyFieldAccepted(t *testing.T) {
	s := newTestSource(t, Config{Key: "wpkh(" + bip84Zpub + "/<0;1>/*)"})
	if a, _ := s.addressAt(1, 0); a != "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el" {
		t.Fatalf("change 0 = %s", a)
	}
	if s.importedVia != "Output descriptor" {
		t.Fatalf("importedVia = %q", s.importedVia)
	}
}
