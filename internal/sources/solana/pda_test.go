package solana

import (
	"bytes"
	"testing"

	"filippo.io/edwards25519"
	"github.com/btcsuite/btcd/btcutil/base58"
)

func b58(t *testing.T, s string) []byte {
	t.Helper()
	b := base58.Decode(s)
	if len(b) != 32 {
		t.Fatalf("%q does not decode to 32 bytes (got %d)", s, len(b))
	}
	return b
}

// The vectors below were derived with an independent pure-Python implementation and then
// confirmed against mainnet:
//   - the ATA below is one of the 23 USDC token accounts returned by getTokenAccountsByOwner
//     for owner 5tzFk...uAi9 (see testdata/token_accounts_usdc.json);
//   - the Metaplex PDA is owned by metaqbxx... and decodes to name "USD Coin", symbol "USDC"
//     (see testdata/metaplex_usdc_account.json).
func TestFindProgramAddressKnownVectors(t *testing.T) {
	tokenProg := b58(t, "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA")
	ataProg := b58(t, "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	metaProg := b58(t, "metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s")
	usdc := b58(t, "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v")
	owner := b58(t, "5tzFkiKscXHK5ZXCGbXZxdw7gTjjD1mBwuoFbhUvuAi9")

	tests := []struct {
		name     string
		seeds    [][]byte
		program  []byte
		wantAddr string
		wantBump uint8
	}{
		{
			name:     "associated token account of owner+USDC (bump search goes below 255)",
			seeds:    [][]byte{owner, tokenProg, usdc},
			program:  ataProg,
			wantAddr: "FzbcyEZ9m8xjtergWgWDq7mfPoHEbboBF791B6cTpzbq",
			wantBump: 254,
		},
		{
			name:     "Metaplex metadata PDA of USDC",
			seeds:    [][]byte{[]byte("metadata"), metaProg, usdc},
			program:  metaProg,
			wantAddr: "5x38Kp4hvdomTCnCrAny4UtMUt5rQBdB6px2K1Ui45Wq",
			wantBump: 255,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pda, bump, err := FindProgramAddress(tc.seeds, tc.program)
			if err != nil {
				t.Fatal(err)
			}
			if got := base58.Encode(pda); got != tc.wantAddr {
				t.Errorf("pda = %s, want %s", got, tc.wantAddr)
			}
			if bump != tc.wantBump {
				t.Errorf("bump = %d, want %d", bump, tc.wantBump)
			}
			// A PDA must never be a valid ed25519 point.
			if _, err := new(edwards25519.Point).SetBytes(pda); err == nil {
				t.Error("PDA is on the ed25519 curve")
			}
		})
	}
}

func TestFindProgramAddressDeterministicAndOffCurve(t *testing.T) {
	prog := b58(t, "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	for i := 0; i < 50; i++ {
		seed := []byte{byte(i), 0xAB, 0xCD}
		a, ba, err := FindProgramAddress([][]byte{seed}, prog)
		if err != nil {
			t.Fatal(err)
		}
		b, bb, _ := FindProgramAddress([][]byte{seed}, prog)
		if !bytes.Equal(a, b) || ba != bb {
			t.Fatalf("seed %d: not deterministic", i)
		}
		if len(a) != 32 {
			t.Fatalf("seed %d: len = %d", i, len(a))
		}
		if _, err := new(edwards25519.Point).SetBytes(a); err == nil {
			t.Fatalf("seed %d: PDA on curve", i)
		}
	}
}

func TestFindProgramAddressRejectsBadInput(t *testing.T) {
	prog := b58(t, "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL")
	if _, _, err := FindProgramAddress([][]byte{make([]byte, 33)}, prog); err == nil {
		t.Error("33-byte seed should be rejected")
	}
	// Solana allows 16 seeds in total and the bump counts as one of them.
	seeds := make([][]byte, 16)
	for i := range seeds {
		seeds[i] = []byte{1}
	}
	if _, _, err := FindProgramAddress(seeds, prog); err == nil {
		t.Error("16 user seeds (+ bump = 17) should be rejected")
	}
	if _, _, err := FindProgramAddress([][]byte{{1}}, prog[:31]); err == nil {
		t.Error("31-byte program id should be rejected")
	}
	// 32-byte seeds and 15 user seeds (+ bump = 16) are the limits and must work.
	max := make([][]byte, 15)
	for i := range max {
		max[i] = bytes.Repeat([]byte{byte(i)}, 32)
	}
	if _, _, err := FindProgramAddress(max, prog); err != nil {
		t.Errorf("15x32-byte seeds: %v", err)
	}
}
