package bitcoin

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/btcsuite/btcd/btcutil"
)

// Keys used by the vectors in BIP380-389 and Bitcoin Core src/test/descriptor_tests.cpp.
const (
	k6ERA  = "xpub6ERApfZwUNrhLCkDtcHTcxd75RbzS1ed54G1LkBUHQVHQKqhMkhgbmJbZRkrgZw4koxb5JaHWkY4ALHY2grBGRjaDMzQLcgJvLJuZZvRcEL"
	k68NZ  = "xpub68NZiKmJWnxxS6aaHmn81bvJeTESw724CRDs6HbuccFQN9Ku14VQrADWgqbhhTHBaohPX4CjNLf9fq9MYo6oDaPPLPxSb7gwQN3ih19Zm4Y"
	k69H7  = "xpub69H7F5d8KSRgmmdJg2KhpAK8SR3DjMwAdkxj3ZuxV27CprR9LgpeyGmXUbC6wb7ERfvrnKZjXoUmmDznezpbZb7ap6r1D3tgFxHmwMkQTPH"
	k69Hz  = "xpub69H7F5dQzmVd3vPuLKtcXJziMEQByuDidnX3YdwgtNsecY5HRGtAAQC5mXTt4dsv9RzyjgDjAQs9VGVV6ydYCHnprc9vvaA5YtqWyL6hyds"
	k661G  = "xpub661MyMwAqRbcGDZQUKLqmWodYLcoBQnQH33yYkkF3jjxeLvY8qr2wWGEWkiKFaaQfJCoi3HeEq3Dc5DptfbCyjD38fNhSqtKc1UHaP4ba3t"
	k661F  = "xpub661MyMwAqRbcFW31YEwpkMuc5THy2PSt5bDMsktWQcFF8syAmRUapSCGu8ED9W6oDMSgv6Zz8idoc4a6mr8BDzTJY47LJhkJ8UB7WEGuduB"
	k6FHa  = "xpub6FHa3pjLCk84BayeJxFW2SP4XRrFd1JYnxeLeU8EqN3vDfZmbqBqaGJAyiLjTAwm6ZLRQUMv1ZACTj37sR62cfN7fe5JnJ7dh8zL4fiyLHV"
	pubA34 = "03a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd"
	pubU34 = "04a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd5b8dec5235a0fa8722476c7709c02559e3aa73aa03918ba2d492eea75abea235"
	pub260 = "0260b2003c386519fc9eadf2b5cf124dd8eea4c4e68d5e154050a9346ea98ce600"
)

// sixteenKeys are the public keys of the BIP383 / Core sh(wsh(multi(16,...))) vector.
var sixteenKeys = []string{
	"03669b8afcec803a0d323e9a17f3ea8e68e8abe5a278020a929adbec52421adbd0",
	"0260b2003c386519fc9eadf2b5cf124dd8eea4c4e68d5e154050a9346ea98ce600",
	"0362a74e399c39ed5593852a30147f2959b56bb827dfa3e60e464b02ccf87dc5e8",
	"0261345b53de74a4d721ef877c255429961b7e43714171ac06168d7e08c542a8b8",
	"02da72e8b46901a65d4374fe6315538d8f368557dda3a1dcf9ea903f3afe7314c8",
	"0318c82dd0b53fd3a932d16e0ba9e278fcc937c582d5781be626ff16e201f72286",
	"0297ccef1ef99f9d73dec9ad37476ddb232f1238aff877af19e72ba04493361009",
	"02e502cfd5c3f972fe9a3e2a18827820638f96b6f347e54d63deb839011fd5765d",
	"03e687710f0e3ebe81c1037074da939d409c0025f17eb86adb9427d28f0f7ae0e9",
	"02c04d3a5274952acdbc76987f3184b346a483d43be40874624b29e3692c1df5af",
	"02ed06e0f418b5b43a7ec01d1d7d27290fa15f75771cb69b642a51471c29c84acd",
	"036d46073cbb9ffee90473f3da429abc8de7f8751199da44485682a989a4bebb24",
	"02f5d1ff7c9029a80a4e36b9a5497027ef7f3e73384a4a94fbfe7c4e9164eec8bc",
	"02e41deffd1b7cce11cde209a781adcffdabd1b91c0ba0375857a2bfd9302419f3",
	"02d76625f7956a7fc505ab02556c23ee72d832f1bac391bcd2d3abce5710a13d06",
	"0399eb0a5487515802dc14544cf10b3666623762fbed2ec38a3975716e2c29c232",
}

// Core multipath vector: multi(2,k6ERA/<1;2>/*,k68NZ/<3;4>/0/*), scripts at index 0..2 per path.
var coreMultipathMulti = [][]string{
	{
		"522103095e95d8c50ae3f3fea93fa8e983f710489f60ff681a658c06eba64622c824b121020443e9e729b42628913f1a69b46b7d43ff87c46e86140e12ee420d7e2e8caf8c52ae",
		"5221027512d6bd74e24eeb1ad752d5be800adc5886ded11c5293a9a701db83658b526a2102371e912dea5fefa56158908fe4c9f66bc925a8939b10f3821e8f8be797b9ca8252ae",
		"522102cc9fd211dc0a1c8bb7a106ff831be0e253bc992f21d08fb8a6fd43fae51b9b892103e43eddc68afc9746c9d09ce0bf8067b4f2416287abbc422ed1ac300673b1104952ae",
	},
	{
		"5221031c0517fff3d483f06ca769bd2326bf30aca1c4de278e676e6ef760c3301244c6210316e171ff4f82dc62ad3f0d84c97865034fc5041eaa508b48c1d7af77f301c8bd52ae",
		"52210240f010ccff4202ade2ef87756f6b9af57bbf5ebcb0393b949e6e5d45d30bff36210229057a7e03510b8cb66727fab3f47a52a02ea94eae03e7c2e81b72a26781bfde52ae",
		"5221034052522058a07b647bd08fa1a9eaedae0222eac76ddd122ff8096ec969398de721038cb8180dd4c956848bcf191e45aaf297146207559fb8737881156aadaf13704152ae",
	},
}

// BIP383 / Core: sortedmulti(2,k6ERA/*,k68NZ/0/0/*), scripts at index 0..2.
var bip383SortedMulti = []string{
	"5221025d5fc65ebb8d44a5274b53bac21ff8307fec2334a32df05553459f8b1f7fe1b62102fbd47cc8034098f0e6a94c6aeee8528abf0a2153a5d8e46d325b7284c046784652ae",
	"52210264fd4d1f5dea8ded94c61e9641309349b62f27fbffe807291f664e286bfbe6472103f4ece6dfccfa37b211eb3d0af4d0c61dba9ef698622dc17eecdf764beeb005a652ae",
	"5221022ccabda84c30bad578b13c89eb3b9544ce149787e5b538175b1d1ba259cbb83321024d902e1a2fc7a8755ab5b694c575fce742c48d9ff192e63df5193e4c7afe1f9c52ae",
}

func mustParse(t *testing.T, s string) *descriptor {
	t.Helper()
	d, err := parseDescriptor(s)
	if err != nil {
		t.Fatalf("parseDescriptor(%.60s…): %v", s, err)
	}
	return d
}

func spkHex(t *testing.T, d *descriptor, path int, i uint32) string {
	t.Helper()
	b, err := d.scriptPubKey(path, i)
	if err != nil {
		t.Fatalf("scriptPubKey(%d,%d): %v", path, i, err)
	}
	return hex.EncodeToString(b)
}

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// wshOf returns the P2WSH scriptPubKey for a witness script: OP_0 <sha256(script)>.
func wshOf(t *testing.T, script string) string {
	sum := sha256.Sum256(unhex(t, script))
	return "0020" + hex.EncodeToString(sum[:])
}

// shOf returns the P2SH scriptPubKey for a redeem script: OP_HASH160 <hash160> OP_EQUAL.
func shOf(t *testing.T, script string) string {
	return "a914" + hex.EncodeToString(btcutil.Hash160(unhex(t, script))) + "87"
}

func TestChecksumBIP380(t *testing.T) {
	sum, err := descChecksum("raw(deadbeef)")
	if err != nil || sum != "89f8spxm" {
		t.Fatalf("descChecksum(raw(deadbeef)) = %q, %v; want 89f8spxm", sum, err)
	}
	for _, ok := range []string{"raw(deadbeef)#89f8spxm", "raw(deadbeef)"} {
		body, err := stripChecksum(ok)
		if err != nil || body != "raw(deadbeef)" {
			t.Errorf("stripChecksum(%q) = %q, %v", ok, body, err)
		}
	}
	bad := map[string]string{
		"missing checksum":   "raw(deadbeef)#",
		"too long checksum":  "raw(deadbeef)#89f8spxmx",
		"too short checksum": "raw(deadbeef)#89f8spx",
		"error in payload":   "raw(deedbeef)#89f8spxm",
		"error in checksum":  "raw(deedbeef)##9f8spxm",
		"invalid characters": "raw(Ü)#00000000",
	}
	for name, s := range bad {
		if _, err := stripChecksum(s); err == nil {
			t.Errorf("%s: stripChecksum(%q) accepted", name, s)
		}
	}
}

func TestChecksumCoreVectors(t *testing.T) {
	body := "sh(multi(2,[00000000/111'/222]" + k6ERA + "," + k68NZ + "/0))"
	bodyH := "sh(multi(2,[00000000/111h/222]" + k6ERA + "," + k68NZ + "/0))"
	for _, s := range []string{body + "#tjg09x5t", bodyH + "#hgmsckna"} {
		if got := spkHex(t, mustParse(t, s), 0, 0); got != "a91445a9a622a8b0a1269944be477640eedc447bbd8487" {
			t.Errorf("%s: spk %s", s[len(s)-9:], got)
		}
	}
	for _, s := range []string{
		strings.Replace(body, "multi(2", "multi(3", 1) + "#tjg09x5t", // error in payload
		body + "#tjq09x4t", // error in checksum
		body + "##tjq09x4t",
		body + "#",
	} {
		if _, err := parseDescriptor(s); err == nil || !strings.Contains(err.Error(), "checksum") {
			t.Errorf("want checksum error for …%s, got %v", s[len(s)-10:], err)
		}
	}
}

func TestDescriptorScriptVectors(t *testing.T) {
	type vec struct {
		name  string
		desc  string
		paths [][]string // scripts per multipath index, then per child index 0..n
	}
	shwsh16 := "sh(wsh(multi(16," + strings.Join(sixteenKeys, ",") + ")))"
	vectors := []vec{
		// BIP381
		{"BIP381 pkh origin '", "pkh([deadbeef/1/2'/3/4']" + pubA34 + ")", [][]string{{"76a9149a1c78a507689f6f54b847ad1cef1e614ee23f1e88ac"}}},
		{"BIP381 pkh origin h", "pkh([deadbeef/1/2h/3/4h]" + pubA34 + ")", [][]string{{"76a9149a1c78a507689f6f54b847ad1cef1e614ee23f1e88ac"}}},
		{"pkh origin H (accepted, unlike BIP380)", "pkh([deadbeef/1/2H/3/4H]" + pubA34 + ")", [][]string{{"76a9149a1c78a507689f6f54b847ad1cef1e614ee23f1e88ac"}}},
		{"BIP381 pkh uncompressed", "pkh(" + pubU34 + ")", [][]string{{"76a914b5bd079c4d57cc7fc28ecf8213a6b791625b818388ac"}}},
		{"BIP381 pkh xpub child", "pkh([bd16bee5/2147483647h]" + k69Hz + "/0)", [][]string{{"76a914ebdc90806a9c4356c1c88e42216611e1cb4c1c1788ac"}}},
		// BIP382 / Core
		{"BIP382 wpkh hex", "wpkh(" + pubA34 + ")", [][]string{{"00149a1c78a507689f6f54b847ad1cef1e614ee23f1e"}}},
		{"BIP382 wpkh ranged", "wpkh([ffffffff/13']" + k69H7 + "/1/2/*)", [][]string{{
			"0014326b2249e3a25d5dc60935f044ee835d090ba859",
			"0014af0bd98abc2f2cae66e36896a39ffe2d32984fb7",
			"00141fa798efd1cbf95cebf912c031b8a4a6e9fb9f27",
		}}},
		{"Core sh(wpkh) hex", "sh(wpkh(" + pubA34 + "))", [][]string{{"a91484ab21b1b2fd065d4504ff693d832434b6108d7b87"}}},
		// BIP383 / Core multisig
		{"BIP383 sh(multi) origin", "sh(multi(2,[00000000/111'/222]" + k6ERA + "," + k68NZ + "/0))", [][]string{{"a91445a9a622a8b0a1269944be477640eedc447bbd8487"}}},
		{"Core wsh(multi) xpub+hex", "wsh(multi(1," + k6FHa + "/0," + pubA34 + "))", [][]string{{"0020cb155486048b23a6da976d4c6fe071a2dbc8a7b57aaf225b8955f2e2a27b5f00"}}},
		{"BIP383 sh(wsh(multi(16)))", shwsh16, [][]string{{"a9147fc63e13dc25e8a95a3cee3d9a714ac3afd96f1e87"}}},
		{"BIP383 sortedmulti hex (BIP67 order)", "sortedmulti(1," + pubU34 + "," + pubA34 + ")", [][]string{{
			"512103a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd4104a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd5b8dec5235a0fa8722476c7709c02559e3aa73aa03918ba2d492eea75abea23552ae",
		}}},
		{"BIP383 sortedmulti ranged", "sortedmulti(2," + k6ERA + "/*," + k68NZ + "/0/0/*)", [][]string{bip383SortedMulti}},
		// BIP389 / Core multipath
		{"BIP389 multi <1;2> <3;4>", "multi(2," + k6ERA + "/<1;2>/*," + k68NZ + "/<3;4>/0/*)", coreMultipathMulti},
		{"BIP389 wpkh <1;3> mid-path", "wpkh([ffffffff/13h]" + k69H7 + "/<1;3>/2/*)", [][]string{
			{"0014326b2249e3a25d5dc60935f044ee835d090ba859", "0014af0bd98abc2f2cae66e36896a39ffe2d32984fb7", "00141fa798efd1cbf95cebf912c031b8a4a6e9fb9f27"},
			{"001426183882ef9c76b9a44386e9b387f33cee7c3a2d", "001447c1b9dc215c3f8b47e572981eb97528768cde4e", "00146e92cbaa397f9caeccf9a049460258af6ccd67e2"},
		}},
		{"BIP389 sh(multi) 3 paths", "sh(multi(2," + k6ERA + "/<1;2;3>/0/*," + k68NZ + "/0/*," + k661G + "/0/0/<3;4;5>/*))", [][]string{
			{"a914689cdf7de5836ec04fb971d128cc84858f73e11487", "a9142ea7dbaf0a77ee19f080cdacb3e13560e3cd9cf587", "a9143da854021f58f5e2d3ff6bb4fcd0ced877deb34987"},
			{"a9143dd613d162e89b83369bbf08e5f1977cfdc9b02787", "a91449eef5d3df5c465b20a630c66058fe689082d8e187", "a91492be56babf54ea2109c577f799ba6d73948e8c3287"},
			{"a9140093ca92097bdf557fbb0570bb77e1efd2e7529c87", "a914e4d0419d3d2ce8f921a800796811ff5462bb151887", "a914997bf69841ac444190dc02f5e6031dd6f8feab4587"},
		}},
		// BIP386
		{"BIP386 tr x-only", "tr(a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd)", [][]string{{"512077aab6e066f8a7419c5ab714c12c67d25007ed55a43cadcacb4d7a970a093f11"}}},
	}

	// Wrapped forms of Core's bare multisig vectors: the witness/redeem script must be
	// exactly Core's script, so the expected outputs are hashed from Core's hex here.
	wrapped := func(name, wrap string, f func(*testing.T, string) string, inner string, scripts [][]string) vec {
		out := make([][]string, len(scripts))
		for p := range scripts {
			for _, sc := range scripts[p] {
				out[p] = append(out[p], f(t, sc))
			}
		}
		return vec{name, strings.Replace(wrap, "X", inner, 1), out}
	}
	shwsh := func(t *testing.T, sc string) string { return shOf(t, wshOf(t, sc)) }
	mp := "multi(2," + k6ERA + "/<1;2>/*," + k68NZ + "/<3;4>/0/*)"
	sm := "sortedmulti(2," + k6ERA + "/*," + k68NZ + "/0/0/*)"
	vectors = append(vectors,
		wrapped("wsh(multi) multipath", "wsh(X)", wshOf, mp, coreMultipathMulti),
		wrapped("sh(wsh(multi)) multipath", "sh(wsh(X))", shwsh, mp, coreMultipathMulti),
		wrapped("wsh(sortedmulti) ranged", "wsh(X)", wshOf, sm, [][]string{bip383SortedMulti}),
		wrapped("sh(sortedmulti) ranged", "sh(X)", shOf, sm, [][]string{bip383SortedMulti}),
	)

	for _, v := range vectors {
		t.Run(v.name, func(t *testing.T) {
			d := mustParse(t, v.desc)
			if d.npaths != len(v.paths) {
				t.Fatalf("npaths = %d, want %d", d.npaths, len(v.paths))
			}
			for p, scripts := range v.paths {
				for i, want := range scripts {
					if got := spkHex(t, d, p, uint32(i)); got != want {
						t.Errorf("path %d index %d: got %s, want %s", p, i, got, want)
					}
				}
			}
		})
	}
}

func TestDescriptorAddressesFromBIPAccounts(t *testing.T) {
	tests := []struct {
		desc  string
		path  int
		idx   uint32
		want  string
		label string
		deriv string
	}{
		{"pkh(" + bip44Xpub + "/0/*)", 0, 0, "1LqBGSKuX5yYUonjxT5qGfpUsXKYYWeabA", "Legacy (p2pkh)", ""},
		{"sh(wpkh(" + bip49Ypub + "/0/*))", 0, 0, "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf", "Nested SegWit (p2sh-p2wpkh)", ""},
		{"wpkh([73c5da0a/84'/0'/0']" + bip84Zpub + "/<0;1>/*)", 1, 0, "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el", "Native SegWit (p2wpkh)", "m/84'/0'/0'"},
		{"tr([73c5da0a/86h/0h/0h]" + bip86Xpub + "/<0;1>/*)", 0, 1, "bc1p4qhjn9zdvkux4e44uhx8tc55attvtyu358kutcqkudyccelu0was9fqzwh", "Taproot (p2tr)", "m/86'/0'/0'"},
		{"tr([73c5da0a/86h/0h/0h]" + bip86Xpub + "/<0;1>/*)", 1, 0, "bc1p3qkhfews2uk44qtvauqyr2ttdsw7svhkl9nkm9s9c3x4ax5h60wqwruhk7", "Taproot (p2tr)", "m/86'/0'/0'"},
		{"addr(bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu)", 0, 0, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu", "Native SegWit (p2wpkh)", ""},
		{"wsh(sortedmulti(2,[f57ec65d/48h/0h/0h/2h]" + k6ERA + "/<0;1>/*," + k68NZ + "/<0;1>/*," + k69H7 + "/<0;1>/*))", 0, 0, "", "2-of-3 multisig (p2wsh)", "m/48'/0'/0'/2'"},
		{"sh(wsh(multi(1," + k6ERA + "/*," + k68NZ + "/*)))", 0, 0, "", "1-of-2 multisig (p2sh-p2wsh)", ""},
		{"sh(sortedmulti(2," + k6ERA + "/*," + k68NZ + "/*))", 0, 0, "", "2-of-2 multisig (p2sh)", ""},
	}
	for _, tc := range tests {
		d := mustParse(t, tc.desc)
		addr, err := d.address(tc.path, tc.idx)
		if err != nil {
			t.Errorf("%.40s: address: %v", tc.desc, err)
			continue
		}
		if tc.want != "" && addr.EncodeAddress() != tc.want {
			t.Errorf("%.40s: address(%d,%d) = %s, want %s", tc.desc, tc.path, tc.idx, addr.EncodeAddress(), tc.want)
		}
		if got := d.label(); got != tc.label {
			t.Errorf("%.40s: label = %q, want %q", tc.desc, got, tc.label)
		}
		if got := d.derivation(); got != tc.deriv {
			t.Errorf("%.40s: derivation = %q, want %q", tc.desc, got, tc.deriv)
		}
	}
}

func TestInvalidDescriptors(t *testing.T) {
	bad := map[string]string{
		// BIP380 key expressions
		"children in origin":      "pkh([deadbeef/0h/0h/0h/*]" + pub260 + ")",
		"trailing slash origin":   "pkh([deadbeef/0h/0h/0h/]" + pub260 + ")",
		"short fingerprint":       "pkh([deadbef/0h/0h/0h]" + pub260 + ")",
		"long fingerprint":        "pkh([deadbeeef/0h/0h/0h]" + pub260 + ")",
		"bad hardened f":          "pkh([deadbeef/0f/0f/0f]" + pub260 + ")",
		"bad hardened -":          "pkh([deadbeef/-0/-0/-0]" + pub260 + ")",
		"index out of range":      "pkh(" + k661F + "/2147483648)",
		"invalid index":           "pkh(" + k661F + "/1aa)",
		"multiple origins":        "pkh([aaaaaaaa][aaaaaaaa]" + k661F + "/0)",
		"missing origin start":    "pkh(aaaaaaaa]" + k661F + "/0)",
		"non hex fingerprint":     "pkh([gaaaaaaa]" + k661F + "/0)",
		"origin without key":      "pkh([deadbeef])",
		"hex key with derivation": "pkh(" + pubA34 + "/0)",
		"hardened after xpub":     "wpkh(" + k661F + "/0h/*)",
		"hardened wildcard":       "wpkh(" + k661F + "/0/*h)",
		"wildcard not last":       "wpkh(" + k661F + "/*/0)",
		// BIP389 multipath
		"two multipath steps":  "pkh(" + k661F + "/<0;1>/<2;3>)",
		"multipath in origin":  "pkh([deadbeef/<0;1>]" + k661F + "/0)",
		"mismatched multipath": "sh(multi(2," + k6ERA + "/<1;2;3>/0/*," + k68NZ + "/0/*," + k661G + "/0/0/<3;4>/*))",
		"empty multipath":      "wpkh(" + k661F + "/<>/*)",
		"missing mp start":     "wpkh(" + k661F + "/0>/*)",
		"missing mp end":       "wpkh(" + k661F + "/<0/*)",
		"missing mp index":     "wpkh(" + k661F + "/<0;>/*)",
		// BIP381 / BIP382
		"sh of key":              "sh(" + pubA34 + ")",
		"sh in sh":               "sh(sh(pkh(" + pubA34 + ")))",
		"pkh of script":          "pkh(pk(" + pubA34 + "))",
		"wpkh uncompressed":      "wpkh(" + pubU34 + ")",
		"sh(wpkh) uncompressed":  "sh(wpkh(" + pubU34 + "))",
		"wsh multi uncompressed": "wsh(multi(1," + pubU34 + "))",
		"wpkh in wsh":            "wsh(wpkh(" + pubA34 + "))",
		"wsh in wsh":             "wsh(wsh(pkh(" + pubA34 + ")))",
		"script in wpkh":         "wpkh(wsh(pkh(" + pubA34 + ")))",
		"key in wsh":             "wsh(" + pubA34 + ")",
		// BIP383
		"16 keys in p2sh":   "sh(multi(16," + strings.Join(sixteenKeys, ",") + "))",
		"threshold not num": "multi(a," + pubA34 + "," + pubU34 + ")",
		"threshold zero":    "multi(0," + pubA34 + "," + pubU34 + ")",
		"threshold > keys":  "multi(3," + pubA34 + "," + pub260 + ")",
		// BIP386
		"tr uncompressed": "tr(" + pubU34 + ")",
		"tr in wsh":       "wsh(tr(a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd))",
		"tr in sh":        "sh(tr(a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd))",
		// unsupported
		"pk":       "pk(" + pubA34 + ")",
		"combo":    "combo(" + pubA34 + ")",
		"raw":      "raw(deadbeef)",
		"garbage":  "hello",
		"unclosed": "wpkh(" + k661F + "/0/*",
		"trailing": "wpkh(" + pubA34 + ")x",
		"empty":    "",
	}
	for name, s := range bad {
		if _, err := parseDescriptor(s); err == nil {
			t.Errorf("%s: accepted %q", name, s)
		}
	}
}

func TestDescriptorSpecificErrors(t *testing.T) {
	cases := []struct{ desc, want string }{
		{"tr(a34b99f22c790c4e36b2b3c2c35a36db06226e41c692fc82b8b56ac1c540c5bd,pk(669b8afcec803a0d323e9a17f3ea8e68e8abe5a278020a929adbec52421adbd0))", "script tree"},
		{"pkh(" + wifKey + ")", "private key"},
		{"wpkh(" + bip86Xprv + "/0/*)", "private key"},
		{"wpkh(" + bip49Upub + "/0/*)", "mainnet only"},
		{"addr(2Mww8dCYPUpKHofjgcXcBCEGmniw9CoaiD2)", "mainnet only"},
		{"wpkh(" + k661F + "/0h/*)", "hardened"},
		{"combo(" + pubA34 + ")", "unsupported"},
	}
	for _, c := range cases {
		_, err := parseDescriptor(c.desc)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%.30s: want error containing %q, got %v", c.desc, c.want, err)
		}
	}
}

func TestDescriptorErrorsDoNotLeakKeys(t *testing.T) {
	_, err := parseDescriptor("wpkh(" + bip84Zpub + "/0h/*)")
	if err == nil {
		t.Fatal("want error")
	}
	if strings.Contains(err.Error(), bip84Zpub) {
		t.Fatalf("error leaks key: %v", err)
	}
}

func TestDescriptorSourceChains(t *testing.T) {
	recv := "wpkh(" + bip84Zpub + "/0/*)"
	change := "wpkh(" + bip84Zpub + "/1/*)"
	cases := map[string]string{
		"multipath":                     "wpkh([73c5da0a/84h/0h/0h]" + bip84Zpub + "/<0;1>/*)",
		"two lines":                     recv + "\n" + change,
		"two lines with comments":       "# receive\n" + recv + "\n\n# change\n" + change + "\n",
		"receive only, change inferred": recv,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTestSource(t, Config{Descriptor: in})
			r, err := s.addressAt(0, 1)
			if err != nil || r != "bc1qnjg0jd8228aq7egyzacy8cys3knf9xvrerkf9g" {
				t.Fatalf("receive 1 = %s, %v", r, err)
			}
			c, err := s.addressAt(1, 0)
			if err != nil || c != "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el" {
				t.Fatalf("change 0 = %s, %v", c, err)
			}
		})
	}
}

func TestDescriptorSourceRejects(t *testing.T) {
	recv := "wpkh(" + bip84Zpub + "/0/*)"
	cases := map[string]string{
		"three lines":             recv + "\n" + recv + "\n" + recv,
		"multipath plus line":     "wpkh(" + bip84Zpub + "/<0;1>/*)\n" + recv,
		"bare multisig":           "multi(1," + k6ERA + "/*," + k68NZ + "/*)",
		"pair of different kinds": recv + "\npkh(" + bip84Zpub + "/1/*)",
	}
	for name, in := range cases {
		if _, err := New(Config{Descriptor: in, EsploraURL: "http://x"}, nil); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestNonRangedDescriptorIsSingleAddress(t *testing.T) {
	s := newTestSource(t, Config{Descriptor: "wpkh(" + pubA34 + ")"})
	a, err := s.addressAt(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := btcutil.NewAddressWitnessPubKeyHash(unhex(t, "9a1c78a507689f6f54b847ad1cef1e614ee23f1e"), mainnet)
	if a != want.EncodeAddress() {
		t.Fatalf("got %s want %s", a, want.EncodeAddress())
	}
	if _, err := s.addressAt(0, 1); err == nil {
		t.Fatal("single-address wallet derived index 1")
	}
}
