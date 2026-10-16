package solana

import (
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/btcsuite/btcd/btcutil/base58"
)

// metaplexKeyMetadataV1 is the account discriminator ("Key") of a Token Metadata account.
const metaplexKeyMetadataV1 = 4

// metadataPDA returns the address of the Metaplex Token Metadata account of mint:
// PDA(["metadata", metadataProgram, mint], metadataProgram).
func metadataPDA(mint string) (string, error) {
	m, err := decodePubkey(mint)
	if err != nil {
		return "", fmt.Errorf("mint %q: %w", mint, err)
	}
	prog := base58.Decode(MetadataProgramID)
	pda, _, err := FindProgramAddress([][]byte{[]byte("metadata"), prog, m}, prog)
	if err != nil {
		return "", err
	}
	return base58.Encode(pda), nil
}

type metaplexMetadata struct {
	Mint, Name, Symbol, URI string
}

// decodeMetaplexMetadata reads the leading, fixed part of a Token Metadata account (Borsh):
//
//	key u8 | update_authority [32] | mint [32] | name str | symbol str | uri str | ...
//
// where str is a u32 little-endian length followed by that many bytes. Strings are padded with
// NULs by the program, so they are cleaned up. Anything after the URI is ignored.
func decodeMetaplexMetadata(b []byte) (metaplexMetadata, error) {
	const header = 1 + 32 + 32
	if len(b) < header {
		return metaplexMetadata{}, errors.New("metadata account too short")
	}
	if b[0] != metaplexKeyMetadataV1 {
		return metaplexMetadata{}, fmt.Errorf("unexpected metadata account key %d", b[0])
	}
	m := metaplexMetadata{Mint: base58.Encode(b[33:65])}
	off := header
	str := func() (string, error) {
		if len(b)-off < 4 {
			return "", errors.New("metadata account truncated")
		}
		n := uint64(binary.LittleEndian.Uint32(b[off:]))
		off += 4
		if n > uint64(len(b)-off) {
			return "", errors.New("metadata string overruns the account")
		}
		s := string(b[off : off+int(n)])
		off += int(n)
		return s, nil
	}
	var err error
	var name, symbol, uri string
	if name, err = str(); err != nil {
		return metaplexMetadata{}, err
	}
	if symbol, err = str(); err != nil {
		return metaplexMetadata{}, err
	}
	if uri, err = str(); err != nil {
		return metaplexMetadata{}, err
	}
	m.Name = cleanText(name, maxNameRunes)
	m.Symbol = cleanText(symbol, maxSymbolRunes)
	m.URI = cleanText(uri, 200)
	return m, nil
}

// decodeBase64Account decodes the ["<data>", "base64"] pair returned by getAccountInfo.
func decodeBase64Account(data []string) ([]byte, error) {
	if len(data) < 1 {
		return nil, errors.New("account data missing")
	}
	if len(data) >= 2 && data[1] != "base64" {
		return nil, fmt.Errorf("unexpected account encoding %q", data[1])
	}
	return base64.StdEncoding.DecodeString(data[0])
}
