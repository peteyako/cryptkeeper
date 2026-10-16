package solana

import (
	"crypto/sha256"
	"errors"
	"fmt"

	"filippo.io/edwards25519"
	"github.com/btcsuite/btcd/btcutil/base58"
)

// Well-known program and mint addresses.
const (
	TokenProgramID           = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	Token2022ProgramID       = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	AssociatedTokenProgramID = "ATokenGPvbdGVxr1b2hvZbsiqW5xWH25efTNsLJA8knL"
	MetadataProgramID        = "metaqbxxUerdq28cj1RbAWkYQm3ybzjb6a8bt518x1s" // Metaplex Token Metadata
	StakeProgramID           = "Stake11111111111111111111111111111111111111"
	WrappedSOLMint           = "So11111111111111111111111111111111111111112"
)

const (
	maxSeedLen = 32
	// Solana allows 16 seeds per derivation and the bump seed is one of them.
	maxUserSeeds = 15
)

// FindProgramAddress derives a program derived address (PDA) the way
// solana_program::Pubkey::find_program_address does: it hashes
// seeds || bump || programID || "ProgramDerivedAddress" with SHA-256, trying bump seeds from 255
// downwards, and returns the first hash that is NOT a valid ed25519 point.
//
// It is exported so that DeFi adapters can derive position and pool accounts.
func FindProgramAddress(seeds [][]byte, programID []byte) (pda []byte, bump uint8, err error) {
	if len(programID) != 32 {
		return nil, 0, fmt.Errorf("solana: program id must be 32 bytes, got %d", len(programID))
	}
	if len(seeds) > maxUserSeeds {
		return nil, 0, fmt.Errorf("solana: %d seeds exceeds the maximum of %d", len(seeds), maxUserSeeds)
	}
	for i, s := range seeds {
		if len(s) > maxSeedLen {
			return nil, 0, fmt.Errorf("solana: seed %d is %d bytes, maximum is %d", i, len(s), maxSeedLen)
		}
	}
	// Like the reference implementation, bump seeds 255..1 are tried.
	for b := 255; b >= 1; b-- {
		h := sha256.New()
		for _, s := range seeds {
			h.Write(s)
		}
		h.Write([]byte{byte(b)})
		h.Write(programID)
		h.Write([]byte("ProgramDerivedAddress"))
		sum := h.Sum(nil)
		if !isOnCurve(sum) {
			return sum, uint8(b), nil
		}
	}
	return nil, 0, errors.New("solana: no viable bump seed")
}

// isOnCurve reports whether b decodes to a point on the ed25519 curve. The decoding accepts the
// same set of encodings as curve25519-dalek, which Solana uses for this check.
func isOnCurve(b []byte) bool {
	_, err := new(edwards25519.Point).SetBytes(b)
	return err == nil
}

// decodePubkey decodes a base58 string that must be exactly 32 bytes.
func decodePubkey(s string) ([]byte, error) {
	b := base58.Decode(s)
	if len(b) != 32 {
		return nil, errors.New("not a 32-byte base58 public key")
	}
	return b, nil
}
