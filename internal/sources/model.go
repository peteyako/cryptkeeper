// Package sources manages the user's connected sources (exchanges, wallets, manual
// holdings): the data/sources.yaml store, secret sealing, and building live core.Source
// instances through a factory.
package sources

import (
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/sources/manual"
)

// Source types.
const (
	TypeCoinbase = "coinbase"
	TypeKraken   = "kraken"
	TypeBitcoin  = "bitcoin"
	TypeSolana   = "solana"
	TypeManual   = "manual"
	// TypeBrokerage is a brokerage account typed in by hand: tickers and share counts.
	TypeBrokerage = "brokerage"
)

// MaxDescription is the longest description a source may have, in characters.
const MaxDescription = 280

// Types lists every supported type in display order.
var Types = []string{TypeCoinbase, TypeKraken, TypeBitcoin, TypeSolana, TypeManual, TypeBrokerage}

// HandEntered reports whether sources of type t are typed in by hand, so they have no
// activity to sync.
func HandEntered(t string) bool { return t == TypeManual || t == TypeBrokerage }

// Def is one entry of data/sources.yaml. Secret fields hold sealed values ("enc:v1:…").
type Def struct {
	ID    string    `yaml:"id"`
	Type  string    `yaml:"type"`
	Label string    `yaml:"label"`
	Kind  core.Kind `yaml:"kind"`
	// Description says what the wallet is for ("Long-term savings, never sold"). Display
	// only: it never reaches the live source.
	Description string `yaml:"description,omitempty"`

	// coinbase
	KeyName    string `yaml:"keyName,omitempty"`
	PrivateKey string `yaml:"privateKey,omitempty"` // secret
	// kraken
	APIKey    string `yaml:"apiKey,omitempty"`    // secret
	APISecret string `yaml:"apiSecret,omitempty"` // secret
	// bitcoin
	Key         string `yaml:"key,omitempty"`        // xpub/ypub/zpub or address
	Descriptor  string `yaml:"descriptor,omitempty"` // output descriptor (also the result of a wallet-file import)
	ScriptType  string `yaml:"scriptType,omitempty"`
	ImportedVia string `yaml:"importedVia,omitempty"` // "wallet file" when the descriptor came from an upload
	// solana
	Address string `yaml:"address,omitempty"`
	// JupiterVault is the wallet's Jupiter limit-order vault (Limit V2 keeps the funds of
	// open orders in a custodial account of its own), counted with the wallet when set.
	JupiterVault string `yaml:"jupiterVault,omitempty"`
	// manual, and brokerage (where Asset is a ticker and Amount a number of shares)
	Holdings []manual.Holding `yaml:"holdings,omitempty"`

	CreatedAt time.Time `yaml:"createdAt,omitempty"`
}

// File is the on-disk shape of data/sources.yaml.
type File struct {
	Version int   `yaml:"version"`
	Sources []Def `yaml:"sources"`
}

// SecretFields lists the Def fields that are sealed at rest, by form/field name.
var SecretFields = []string{"privateKey", "apiKey", "apiSecret"}

// Secret returns a pointer to the named secret field.
func (d *Def) Secret(name string) *string {
	switch name {
	case "privateKey":
		return &d.PrivateKey
	case "apiKey":
		return &d.APIKey
	case "apiSecret":
		return &d.APISecret
	}
	return nil
}

var idRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,39}$`)

// Validate checks the type-independent shape. Type-specific validation (parsing keys,
// addresses) happens when the factory builds the source.
func (d *Def) Validate() error {
	for _, v := range []string{d.Key, d.Descriptor, d.Address, d.JupiterVault} {
		if LooksLikeSeed(strings.ToLower(strings.TrimSpace(v))) {
			return fmt.Errorf("%s: that looks like a seed phrase or private key; CryptKeep only needs public keys, so it is never stored", d.ID)
		}
	}
	if !idRe.MatchString(d.ID) {
		return fmt.Errorf("id %q: use 1–40 lowercase letters, digits or dashes", d.ID)
	}
	if strings.TrimSpace(d.Label) == "" {
		return fmt.Errorf("%s: label is required", d.ID)
	}
	if utf8.RuneCountInString(d.Description) > MaxDescription {
		return fmt.Errorf("%s: description is longer than %d characters", d.ID, MaxDescription)
	}
	switch d.Kind {
	case core.KindExchange, core.KindHot, core.KindHardware, core.KindManual, core.KindBrokerage:
	default:
		return fmt.Errorf("%s: kind %q must be exchange, hot, hardware, manual or brokerage", d.ID, d.Kind)
	}
	switch d.Type {
	case TypeCoinbase:
		if d.KeyName == "" {
			return fmt.Errorf("%s: Coinbase needs an API key name", d.ID)
		}
		// A missing private key is allowed (an import without secrets): the source then
		// fails to build and its card asks for the key. The add form still requires it.
	case TypeKraken:
		// Same for Kraken's key and secret.
	case TypeBitcoin:
		if d.Key == "" && d.Descriptor == "" {
			return fmt.Errorf("%s: Bitcoin needs an extended public key, descriptor or address", d.ID)
		}
	case TypeSolana:
		if d.Address == "" {
			return fmt.Errorf("%s: Solana needs an address", d.ID)
		}
	case TypeManual:
		if len(d.Holdings) == 0 {
			return fmt.Errorf("%s: add at least one holding", d.ID)
		}
	case TypeBrokerage:
		if len(d.Holdings) == 0 {
			return fmt.Errorf("%s: add at least one holding", d.ID)
		}
		for _, h := range d.Holdings {
			if t, ok := core.NormalizeTicker(h.Asset); !ok || t != h.Asset {
				return fmt.Errorf("%s: %q isn't a ticker (use capitals, e.g. VTI)", d.ID, h.Asset)
			}
		}
	default:
		return fmt.Errorf("%s: unknown type %q", d.ID, d.Type)
	}
	return nil
}

// DefaultKind is the kind a new source of type t gets unless the user picks another.
func DefaultKind(t string) core.Kind {
	switch t {
	case TypeCoinbase, TypeKraken:
		return core.KindExchange
	case TypeBitcoin:
		return core.KindHardware
	case TypeSolana:
		return core.KindHot
	case TypeBrokerage:
		return core.KindBrokerage
	}
	return core.KindManual
}

// Slug turns a label into an id candidate: "Cold storage" → "cold-storage".
func Slug(label string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(label) {
		switch {
		case r >= 'a' && r <= 'z' || r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case b.Len() > 0 && !dash:
			b.WriteByte('-')
			dash = true
		}
	}
	s := strings.Trim(b.String(), "-")
	if len(s) > 40 {
		s = strings.Trim(s[:40], "-")
	}
	if s == "" {
		s = "source"
	}
	return s
}

// LooksLikeSeed catches the most damaging paste mistake: a 12/18/24-word mnemonic or an
// xprv/yprv/zprv private key (bare or inside a descriptor) where a public key belongs.
// Callers pass lower-cased input.
func LooksLikeSeed(s string) bool {
	if strings.HasPrefix(s, "xprv") || strings.HasPrefix(s, "yprv") || strings.HasPrefix(s, "zprv") || strings.Contains(s, "prv") && strings.Contains(s, "(") {
		return true
	}
	words := strings.Fields(s)
	if len(words) != 12 && len(words) != 18 && len(words) != 24 {
		return false
	}
	for _, w := range words {
		for _, c := range w {
			if c < 'a' || c > 'z' {
				return false
			}
		}
	}
	return true
}
