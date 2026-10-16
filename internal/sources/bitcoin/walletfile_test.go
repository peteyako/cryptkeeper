package bitcoin

import (
	"bytes"
	"crypto/sha256"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
)

const bip84AccountXpub = "xpub6CatWdiZiodmUeTDp8LT5or8nmbKNcuyvz7WyksVFkKB4RHwCD3XyuvPEbvqAQY3rAPshWcMLoP2fMFMKHPJ4ZeZXYVUhLv1VMrjPC7PW6V"

// caravanDescriptor is the expected conversion of testdata/caravan.json. Its checksum was
// computed with the BIP380 reference Python implementation, not with this package.
const caravanDescriptor = "wsh(sortedmulti(2," +
	"[f57ec65d/48h/0h/0h/2h]" + k6ERA + "/<0;1>/*," +
	"[0b287198/48h/0h/0h/2h]" + k68NZ + "/<0;1>/*," +
	k69H7 + "/<0;1>/*))#mmzxn56t"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// handMadeP2WSH derives chain/index from each xpub, sorts the keys and builds the 2-of-n
// witness script byte by byte, independently of the descriptor code.
func handMadeP2WSH(t *testing.T, chain, index uint32, xpubs ...string) string {
	t.Helper()
	var keys [][]byte
	for _, x := range xpubs {
		k, err := hdkeychain.NewKeyFromString(x)
		if err != nil {
			t.Fatal(err)
		}
		pub, _ := derivePath(t, k, chain, index).ECPubKey()
		keys = append(keys, pub.SerializeCompressed())
	}
	sort.Slice(keys, func(a, b int) bool { return bytes.Compare(keys[a], keys[b]) < 0 })
	script := []byte{0x52} // OP_2
	for _, k := range keys {
		script = append(append(script, 0x21), k...)
	}
	script = append(script, byte(0x50+len(keys)), 0xae) // OP_n OP_CHECKMULTISIG
	sum := sha256.Sum256(script)
	a, err := btcutil.NewAddressWitnessScriptHash(sum[:], mainnet)
	if err != nil {
		t.Fatal(err)
	}
	return a.EncodeAddress()
}

func TestCaravanToDescriptor(t *testing.T) {
	got, err := DescriptorFromWalletFile(readFixture(t, "caravan.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got != caravanDescriptor {
		t.Fatalf("descriptor:\n got %s\nwant %s", got, caravanDescriptor)
	}
}

func TestCaravanFirstAddresses(t *testing.T) {
	recv0 := handMadeP2WSH(t, 0, 0, k6ERA, k68NZ, k69H7)
	change0 := handMadeP2WSH(t, 1, 0, k6ERA, k68NZ, k69H7)
	// Pinned so a regression in the hand-made helper is noticed too.
	if recv0 != "bc1q6vjz7nkl52vnhss7esrdp3lxpgces4sz9mwggy00dkr0f2yskqss6alch9" {
		t.Fatalf("hand-made receive 0 = %s", recv0)
	}
	for name, cfg := range map[string]Config{
		"wallet file": {WalletFile: readFixture(t, "caravan.json")},
		"descriptor":  {Descriptor: caravanDescriptor},
	} {
		s := newTestSource(t, cfg)
		if a, err := s.addressAt(0, 0); err != nil || a != recv0 {
			t.Errorf("%s: receive 0 = %s, %v; want %s", name, a, err, recv0)
		}
		if a, err := s.addressAt(1, 0); err != nil || a != change0 {
			t.Errorf("%s: change 0 = %s, %v; want %s", name, a, err, change0)
		}
	}
}

func caravanJSON(addressType, network string, required int, xpub string) []byte {
	return []byte(`{"name":"t","addressType":"` + addressType + `","network":"` + network + `",
	"quorum":{"requiredSigners":` + string(rune('0'+required)) + `,"totalSigners":2},
	"extendedPublicKeys":[
	 {"name":"a","bip32Path":"m/48'/0'/0'/1'","xpub":"` + xpub + `","xfp":"f57ec65d"},
	 {"name":"b","bip32Path":"m/48'/0'/0'/1'","xpub":"` + k68NZ + `","xfp":"0b287198"}]}`)
}

func TestCaravanAddressTypes(t *testing.T) {
	cases := map[string]string{
		"P2SH-P2WSH": "sh(wsh(sortedmulti(2,",
		"P2SH_P2WSH": "sh(wsh(sortedmulti(2,",
		"p2wsh":      "wsh(sortedmulti(2,",
		"P2SH":       "sh(sortedmulti(2,",
	}
	for at, prefix := range cases {
		got, err := DescriptorFromWalletFile(caravanJSON(at, "mainnet", 2, k6ERA))
		if err != nil {
			t.Errorf("%s: %v", at, err)
			continue
		}
		if !strings.HasPrefix(got, prefix) {
			t.Errorf("%s: got %s", at, got)
		}
		if _, err := parseDescriptor(got); err != nil {
			t.Errorf("%s: output does not parse: %v", at, err)
		}
	}
}

func TestCaravanNormalizesSLIP132Keys(t *testing.T) {
	zpub := withVersion(t, k6ERA, []byte{0x02, 0xaa, 0x7e, 0xd3}) // Zpub
	got, err := DescriptorFromWalletFile(caravanJSON("P2WSH", "mainnet", 2, zpub))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, k6ERA) || strings.Contains(got, zpub) {
		t.Fatalf("Zpub not rewritten to xpub: %s", got)
	}
}

func TestCaravanRejects(t *testing.T) {
	cases := map[string]struct {
		data []byte
		want string
	}{
		"testnet":          {caravanJSON("P2WSH", "testnet", 2, k6ERA), "mainnet only"},
		"threshold > keys": {caravanJSON("P2WSH", "mainnet", 3, k6ERA), "required"},
		"threshold zero":   {caravanJSON("P2WSH", "mainnet", 0, k6ERA), "required"},
		"unknown type":     {caravanJSON("P2TR", "mainnet", 2, k6ERA), "address type"},
		"private key":      {caravanJSON("P2WSH", "mainnet", 2, bip86Xprv), "private key"},
		"empty json":       {[]byte(`{}`), "wallet file"},
		"garbage text":     {[]byte("hello world\nnot a descriptor\n"), "no output descriptor"},
		"empty":            {nil, "empty"},
	}
	for name, c := range cases {
		_, err := DescriptorFromWalletFile(c.data)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: want error containing %q, got %v", name, c.want, err)
		}
	}
}

func TestSparrowTextExport(t *testing.T) {
	got, err := DescriptorFromWalletFile(readFixture(t, "sparrow.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "wpkh([73c5da0a/84h/0h/0h]" + bip84AccountXpub + "/<0;1>/*)#qf45pmyh"
	if got != want {
		t.Fatalf("got %s\nwant %s", got, want)
	}
}

func TestTextExportReceiveChangePair(t *testing.T) {
	got, err := DescriptorFromWalletFile(readFixture(t, "sparrow-pair.txt"))
	if err != nil {
		t.Fatal(err)
	}
	want := "wpkh([73c5da0a/84h/0h/0h]" + bip84AccountXpub + "/0/*)#afwvtk2s\n" +
		"wpkh([73c5da0a/84h/0h/0h]" + bip84AccountXpub + "/1/*)#vatdkr6g"
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	s := newTestSource(t, Config{WalletFile: readFixture(t, "sparrow-pair.txt")})
	if a, _ := s.addressAt(1, 0); a != "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el" {
		t.Fatalf("change 0 = %s", a)
	}
}

func TestJSONWithDescriptorField(t *testing.T) {
	desc := "wpkh([73c5da0a/84h/0h/0h]" + bip84AccountXpub + "/<0;1>/*)#qf45pmyh"
	got, err := DescriptorFromWalletFile([]byte(`{"label":"Spending","blockheight":800000,"descriptor":"` + desc + `"}`))
	if err != nil || got != desc {
		t.Fatalf("got %q, %v", got, err)
	}
}
