package defi

import (
	"context"
	"encoding/hex"
	"math/big"
	"testing"
)

func TestDiscriminators(t *testing.T) {
	// Each value matches the first 8 bytes of the captured mainnet accounts.
	for name, want := range map[string]string{
		"Position":              "aabc8fe47a40f7d0",
		"Whirlpool":             "3f95d10ce1806309",
		"PersonalPositionState": "466f967ee60f1975",
		"PoolState":             "f7ede3f5d7c3de46",
		"Escrow":                "1fd57bbbba16da9b",
		"Locker":                "4af60671f9e44ba9",
	} {
		d := anchorDiscriminator(name)
		if got := hex.EncodeToString(d[:]); got != want {
			t.Errorf("%s: %s, want %s", name, got, want)
		}
	}
	accts, _ := loadAccounts(t, "orca_accounts.json", "raydium_clmm_accounts.json", "jup_escrow_accounts.json", "perps_accounts.json")
	for key, disc := range map[string][8]byte{
		orcaPosIn:             orcaPositionDisc,
		orcaSOLUSDCPool:       orcaWhirlpoolDisc,
		rayPosIn:              raydiumPositionDisc,
		raySOLUSDCPool:        raydiumPoolDisc,
		escrowCases[0].escrow: jupEscrowDisc,
		"GvJ3DnBxGtxpqVhjUi8L8ekPwVLpNU4DoayN5cRATuK5": perpsPositionDisc,
	} {
		if !hasDiscriminator(accts[key], disc) {
			t.Errorf("%s: discriminator mismatch", key)
		}
	}
}

func TestFindProgramAddressErrors(t *testing.T) {
	if _, err := findProgramAddress(nil, "short"); err == nil {
		t.Error("bad program id accepted")
	}
	if _, err := findProgramAddress([][]byte{make([]byte, 33)}, OrcaWhirlpoolProgram); err == nil {
		t.Error("long seed accepted")
	}
	if _, err := findProgramAddress(make([][]byte, 16), OrcaWhirlpoolProgram); err == nil {
		t.Error("too many seeds accepted")
	}
}

func TestFormatUnits(t *testing.T) {
	for _, c := range []struct {
		raw  string
		dec  int
		want string
	}{
		{"0", 6, "0"},
		{"1", 9, "0.000000001"},
		{"3750000", 6, "3.75"},
		{"42", 0, "42"},
		{"1000000", 6, "1"},
		{"-1500000", 6, "-1.5"},
		{"18746744073709551615", 18, "18.746744073709551615"},
	} {
		n, _ := new(big.Int).SetString(c.raw, 10)
		if got := formatUnits(n, c.dec); got != c.want {
			t.Errorf("formatUnits(%s, %d) = %q, want %q", c.raw, c.dec, got, c.want)
		}
	}
	if formatUnits(nil, 6) != "0" {
		t.Error("nil amount")
	}
	l := legFromRat("m", big.NewRat(2, 3), 6)
	if l.AmountStr != "0.666666" || l.Amount != 0.666666 {
		t.Errorf("legFromRat = %+v", l)
	}
}

func TestGroupThousands(t *testing.T) {
	for in, want := range map[string]string{
		"0": "0", "999": "999", "1000": "1,000", "1998": "1,998", "1234567.891": "1,234,567.89",
		"12.10": "12.1", "5.001": "5",
	} {
		if got := groupThousands(in); got != want {
			t.Errorf("groupThousands(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMintDecimals(t *testing.T) {
	accts, _ := loadAccounts(t, "orca_accounts.json")
	if d, ok := mintDecimals(accts[wsolMint]); !ok || d != 9 {
		t.Errorf("SOL decimals %d %v", d, ok)
	}
	if d, ok := mintDecimals(accts[usdcMint]); !ok || d != 6 {
		t.Errorf("USDC decimals %d %v", d, ok)
	}
	if _, ok := mintDecimals(accts[usdcMint][:81]); ok {
		t.Error("short mint accepted")
	}
	uninit := make([]byte, mintLen)
	if _, ok := mintDecimals(uninit); ok {
		t.Error("uninitialized mint accepted")
	}
}

func TestAdapters(t *testing.T) {
	names := func(as []Adapter) []string {
		var out []string
		for _, a := range as {
			out = append(out, a.Name())
		}
		return out
	}
	if got := names(Adapters(nil, nil)); len(got) != 3 || got[0] != "jupiter" || got[1] != "orca" || got[2] != "raydium" {
		t.Errorf("default adapters %v", got)
	}
	if got := names(Adapters([]string{" Raydium", "bogus", "orca", "raydium"}, fastClient())); len(got) != 2 || got[0] != "raydium" || got[1] != "orca" {
		t.Errorf("selected adapters %v", got)
	}
}

func TestSymbolFallbacks(t *testing.T) {
	var in Input
	if in.symbol(wsolMint, "") != "SOL" || in.symbol("unknownmint1234567890", "") != "unkn…7890" ||
		in.symbol("x", "WSOL") != "SOL" || in.symbol("abc", "ABC") != "ABC" {
		t.Error("symbol fallbacks")
	}
}

func TestFetchAccountsCountMismatch(t *testing.T) {
	if _, err := fetchAccounts(context.Background(), shortRPC{}, []string{"a", "b"}); err == nil {
		t.Error("mismatched result count accepted")
	}
}

type shortRPC struct{}

func (shortRPC) GetMultipleAccounts(context.Context, []string) ([][]byte, error) {
	return [][]byte{nil}, nil
}

func (shortRPC) GetProgramAccounts(context.Context, string, []Filter) ([]KeyedAccount, error) {
	return nil, nil
}
