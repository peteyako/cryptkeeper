package solana

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"testing"
)

// borshString appends a Borsh string (u32 LE length + bytes).
func borshString(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint32(b, uint32(len(s)))
	return append(b, s...)
}

// buildMetadataAccount builds a Metaplex Token Metadata v1 account the way the program lays
// it out: fixed-width padded strings (name 32, symbol 10, uri 200) preceded by u32 lengths.
func buildMetadataAccount(mint, name, symbol, uri string) []byte {
	pad := func(s string, n int) string {
		for len(s) < n {
			s += "\x00"
		}
		return s
	}
	var b []byte
	b = append(b, 4) // Key::MetadataV1
	b = append(b, make([]byte, 32)...)
	b = append(b, base58Decode32(mint)...)
	b = borshString(b, pad(name, 32))
	b = borshString(b, pad(symbol, 10))
	b = borshString(b, pad(uri, 200))
	b = binary.LittleEndian.AppendUint16(b, 0) // seller_fee_basis_points
	b = append(b, 0, 0)                        // creators None, primary_sale_happened false
	return b
}

func TestDecodeMetaplexLiveUSDC(t *testing.T) {
	raw, err := os.ReadFile("testdata/metaplex_usdc_account.json")
	if err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Value struct {
				Data  []string `json:"data"`
				Owner string   `json:"owner"`
			} `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Result.Value.Owner != "metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s" {
		t.Fatalf("fixture owner = %s", resp.Result.Value.Owner)
	}
	data, err := decodeBase64Account(resp.Result.Value.Data)
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeMetaplexMetadata(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "USD Coin" || m.Symbol != "USDC" {
		t.Errorf("name=%q symbol=%q", m.Name, m.Symbol)
	}
	if m.Mint != "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v" {
		t.Errorf("mint = %s", m.Mint)
	}
}

func TestDecodeMetaplexTrimsPaddingAndJunk(t *testing.T) {
	mint := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	acct := buildMetadataAccount(mint, "  My Token  ", "MYT ", "https://example.com/x.json")
	m, err := decodeMetaplexMetadata(acct)
	if err != nil {
		t.Fatal(err)
	}
	if m.Name != "My Token" || m.Symbol != "MYT" || m.URI != "https://example.com/x.json" {
		t.Errorf("got %+v", m)
	}
}

func TestDecodeMetaplexRejectsMalformed(t *testing.T) {
	mint := "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	good := buildMetadataAccount(mint, "A", "B", "")
	cases := map[string][]byte{
		"empty":           nil,
		"header only":     good[:60],
		"truncated name":  good[:70],
		"wrong key":       append([]byte{3}, good[1:]...),
		"huge name len":   append(append([]byte{}, good[:65]...), 0xFF, 0xFF, 0xFF, 0x7F),
		"truncated after": good[:65+4+32+2],
	}
	for name, b := range cases {
		if _, err := decodeMetaplexMetadata(b); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}

func TestMetadataPDAForUSDC(t *testing.T) {
	got, err := metadataPDA("EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	if err != nil {
		t.Fatal(err)
	}
	if got != "5x38Kp4hvdomTCnCrAny4UtMUt5rQBdB6px2K1Ui45Wq" {
		t.Errorf("metadata PDA = %s", got)
	}
	if _, err := metadataPDA("not-a-mint"); err == nil {
		t.Error("invalid mint should error")
	}
}
