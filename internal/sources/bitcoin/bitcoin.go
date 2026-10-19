// Package bitcoin is the read-only Bitcoin wallet source: xpub/ypub/zpub keys, output
// descriptors (single-sig and multisig), Caravan/Sparrow wallet files and plain addresses,
// scanned through an Esplora HTTP API.
//
// Privacy: full extended keys are never logged or put into error messages.
package bitcoin

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"

	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"golang.org/x/sync/singleflight"
)

// Config configures one Bitcoin wallet source. Exactly one of Key, Descriptor or
// WalletFile must be set.
type Config struct {
	ID, Label  string
	Key        string // xpub/ypub/zpub OR a plain address (bc1…/1…/3…)
	Descriptor string // output descriptor, or "receive descriptor\nchange descriptor"
	WalletFile []byte // Caravan/Unchained JSON or a text file containing a descriptor (Sparrow export)
	ScriptType string // optional override for an extended Key: "p2pkh", "p2sh-p2wpkh", "p2wpkh", "p2tr"
	EsploraURL string // e.g. https://mempool.space/api (no trailing slash); DefaultEsploraURL if empty
	// FallbackURLs are further Esplora servers, asked in order while the ones before them
	// aren't answering.
	FallbackURLs []string
	// Auth is the shared Blockstream API key. When it is set and EsploraURL is a public server,
	// requests go to Blockstream's enterprise server instead, the public ones standing by.
	Auth *BlockstreamAuth
	// ScanStore keeps the last address scan across restarts; optional.
	ScanStore ScanStore
	GapLimit  int // DefaultGapLimit if zero or negative
}

const (
	// DefaultGapLimit is the BIP44 address gap limit.
	DefaultGapLimit = 20
	// DefaultEsploraURL is used when Config.EsploraURL is empty.
	DefaultEsploraURL = "https://mempool.space/api"
)

// Values for the "Imported via" Connection row.
const (
	viaKey        = "xpub / zpub"
	viaDescriptor = "Output descriptor"
	viaAddress    = "Address"
	viaWalletFile = "Wallet file"
)

var (
	_ core.Source           = (*Source)(nil)
	_ core.AddressLister    = (*Source)(nil)
	_ core.UTXOLister       = (*Source)(nil)
	_ core.ReceiveAddresser = (*Source)(nil)
)

// chainSpec says how to derive one chain (0 = receive, 1 = change).
type chainSpec struct {
	d    *descriptor
	path int // multipath alternative of d
}

// Source is a Bitcoin wallet. It implements core.Source, core.AddressLister,
// core.UTXOLister and core.ReceiveAddresser.
type Source struct {
	id          string
	importedVia string
	script      string // e.g. "Native SegWit (p2wpkh)"
	derivation  string // e.g. "m/84'/0'/0'", or "" when unknown
	esplora     string
	node        string
	gap         int
	client      *httpx.Client
	now         func() time.Time

	// servers are the Esplora servers in the order they are asked: the configured one, then
	// the fallbacks. fast is client cut to one short attempt, for every server but the last
	// one asked, so a server that doesn't answer costs seconds.
	servers []esploraServer
	fast    *httpx.Client
	auth    *BlockstreamAuth // nil unless the configured server is a public one
	store   ScanStore

	// autoKey is an xpub read as Legacy because no script type was chosen. Ledger shows an
	// xpub for SegWit and Taproot accounts too, so an empty scan of it earns a hint.
	autoKey *hdkeychain.ExtendedKey

	chains []chainSpec // chains[0] is receive; chains[1], when present, is change
	ranged bool        // false for a single address

	mu         sync.Mutex
	addrs      map[[2]int]string // derived address cache, keyed by (chain, index)
	last       *scanResult
	down       map[string]time.Time // by server base URL: didn't answer, asked last until then
	answeredBy string               // host of the server that answered the last request
	keyUsed    bool                 // the last request was answered through the Blockstream key
	keyErr     string               // why the Blockstream key didn't work last time; "" when it did
	hint       string               // script-type hint for autoKey, once checked
	hintDone   bool
	restored   bool // the saved scan was looked for

	scans singleflight.Group // one scan at a time; concurrent callers share it
}

// New parses and validates cfg. It does no network I/O. A nil client means httpx.New().
func New(cfg Config, c *httpx.Client) (*Source, error) {
	key := strings.TrimSpace(cfg.Key)
	desc := strings.TrimSpace(cfg.Descriptor)
	given := 0
	for _, set := range []bool{key != "", desc != "", len(cfg.WalletFile) > 0} {
		if set {
			given++
		}
	}
	switch given {
	case 0:
		return nil, errors.New("bitcoin source needs an xpub/ypub/zpub, an address, an output descriptor or a wallet file")
	case 1:
	default:
		return nil, errors.New("bitcoin source takes only one of: key, descriptor, wallet file")
	}
	if cfg.ScriptType != "" {
		if _, err := parseScriptType(cfg.ScriptType); err != nil {
			return nil, err
		}
	}
	esplora := strings.TrimRight(strings.TrimSpace(cfg.EsploraURL), "/")
	if esplora == "" {
		esplora = DefaultEsploraURL
	}
	node, err := esploraHost(esplora)
	if err != nil {
		return nil, fmt.Errorf("invalid Esplora URL %q: want e.g. https://mempool.space/api", cfg.EsploraURL)
	}
	if c == nil {
		c = httpx.New()
	}
	c = ownPacing(c)
	s := &Source{
		id:      cfg.ID,
		esplora: esplora,
		node:    node,
		gap:     cfg.GapLimit,
		client:  c,
		now:     time.Now,
		addrs:   map[[2]int]string{},
		down:    map[string]time.Time{},
		store:   cfg.ScanStore,
		servers: []esploraServer{{base: esplora, host: node}},
	}
	if cfg.Auth != nil && publicEsploraHosts[node] {
		s.auth = cfg.Auth
	}
	for _, raw := range cfg.FallbackURLs {
		fb := strings.TrimRight(strings.TrimSpace(raw), "/")
		if fb == "" || fb == esplora {
			continue
		}
		host, err := esploraHost(fb)
		if err != nil {
			return nil, fmt.Errorf("invalid fallback Esplora URL %q: want e.g. https://blockstream.info/api", raw)
		}
		s.servers = append(s.servers, esploraServer{base: fb, host: host})
	}
	if len(s.servers) > 1 {
		s.fast = failFast(c)
	}
	for _, srv := range s.servers {
		if !privateHost(srv.host) {
			httpx.SetPace(srv.host, publicPace)
		}
	}
	if s.gap <= 0 {
		s.gap = DefaultGapLimit
	}

	switch {
	case len(cfg.WalletFile) > 0:
		s.importedVia = viaWalletFile
		text, err := DescriptorFromWalletFile(cfg.WalletFile)
		if err != nil {
			return nil, err
		}
		if s.chains, err = chainsFromDescriptorText(text); err != nil {
			return nil, err
		}
	case desc != "":
		s.importedVia = viaDescriptor
		if s.chains, err = chainsFromDescriptorText(desc); err != nil {
			return nil, err
		}
	default:
		if err := s.initKey(key, cfg.ScriptType); err != nil {
			return nil, err
		}
	}

	d := s.chains[0].d
	s.ranged = d.ranged()
	s.script = d.label()
	if s.derivation == "" {
		s.derivation = d.derivation()
	}
	return s, nil
}

// initKey sets up a source from Config.Key: an extended public key or an address. A
// descriptor pasted into the key field is accepted too.
func (s *Source) initKey(key, scriptOverride string) error {
	switch {
	case looksLikeExtendedKey(key):
		k, v, err := parseExtendedPub(key)
		if err != nil {
			return err
		}
		if v.multisig {
			return errCosigner(v.name)
		}
		st := v.script
		if scriptOverride != "" {
			st, _ = parseScriptType(scriptOverride) // validated in New
		} else if v.name == "xpub" {
			s.autoKey = k
		}
		d := descriptorFromXpub(k, st)
		s.importedVia = viaKey
		s.chains = []chainSpec{{d, 0}, {d, 1}}
		s.derivation = accountPath(k, st)
		return nil
	case isWIF(key):
		return errPrivateKey
	case strings.Contains(key, "("):
		s.importedVia = viaDescriptor
		var err error
		s.chains, err = chainsFromDescriptorText(key)
		return err
	}
	a, err := parseAddress(key)
	if err != nil {
		return err
	}
	s.importedVia = viaAddress
	s.chains = []chainSpec{{&descriptor{kind: kindAddr, addr: a, npaths: 1}, 0}}
	return nil
}

func errCosigner(name string) error {
	return fmt.Errorf("%s is a multisig cosigner key: import the wallet's output descriptor or wallet file instead", name)
}

// Detect classifies user input for the Add-source form without network access. It returns
// "xpub", "ypub", "zpub", "descriptor" or "address". Testnet keys and addresses, private
// keys, multisig cosigner keys and malformed input return an error saying why.
func Detect(input string) (kind string, err error) {
	s := strings.TrimSpace(input)
	switch {
	case s == "":
		return "", errors.New("empty input")
	case strings.HasPrefix(s, "{"):
		return "", errors.New("this looks like a wallet file: upload it as a wallet file instead")
	case strings.ContainsAny(s, "(\n"):
		if _, err := chainsFromDescriptorText(s); err != nil {
			return "", err
		}
		return "descriptor", nil
	case looksLikeExtendedKey(s):
		_, v, err := parseExtendedPub(s)
		if err != nil {
			return "", err
		}
		if v.multisig {
			return "", errCosigner(v.name)
		}
		return v.name, nil
	case isWIF(s):
		return "", errPrivateKey
	}
	if _, err := parseAddress(s); err != nil {
		return "", err
	}
	return "address", nil
}

// accountPath infers m/purpose'/0'/account' for a depth-3 account key with a hardened
// child number; "" otherwise.
func accountPath(k *hdkeychain.ExtendedKey, st scriptType) string {
	if k.Depth() != 3 || k.ChildIndex() < hdkeychain.HardenedKeyStart {
		return ""
	}
	return fmt.Sprintf("m/%d'/0'/%d'", st.bip43Purpose(), k.ChildIndex()-hdkeychain.HardenedKeyStart)
}

// chainsFromDescriptorText parses one descriptor (multipath, receive-only or single
// address) or a "receive\nchange" pair. Blank lines and "#" comment lines are ignored.
func chainsFromDescriptorText(text string) ([]chainSpec, error) {
	var ds []*descriptor
	for n, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		d, err := parseDescriptor(line)
		if err != nil {
			return nil, fmt.Errorf("descriptor line %d: %w", n+1, err)
		}
		if d.kind == kindBareMulti {
			return nil, errors.New("bare multisig descriptors have no address: wrap it in wsh() or sh()")
		}
		ds = append(ds, d)
	}
	switch len(ds) {
	case 0:
		return nil, errors.New("no descriptor found")
	case 1:
		d := ds[0]
		if d.ranged() && (d.npaths >= 2 || d.inferChange()) {
			return []chainSpec{{d, 0}, {d, 1}}, nil
		}
		return []chainSpec{{d, 0}}, nil
	case 2:
		r, c := ds[0], ds[1]
		if r.npaths > 1 || c.npaths > 1 {
			return nil, errors.New("a multipath <0;1> descriptor already covers receive and change: give it on its own")
		}
		if !r.ranged() || !c.ranged() {
			return nil, errors.New("receive and change descriptors must both be ranged (end in /*)")
		}
		if r.kind != c.kind || r.threshold != c.threshold || len(r.keys) != len(c.keys) {
			return nil, errors.New("receive and change descriptors must have the same script type and keys")
		}
		return []chainSpec{{r, 0}, {c, 0}}, nil
	}
	return nil, fmt.Errorf("expected one descriptor, or a receive and a change descriptor; got %d", len(ds))
}

func isWIF(s string) bool {
	_, err := btcutil.DecodeWIF(s)
	return err == nil
}

// ID returns the source id.
func (s *Source) ID() string { return s.id }

// addressAt returns the address at index i of chain (0 = receive, 1 = change). It is the one
// place that knows how a wallet derives addresses; results are cached.
func (s *Source) addressAt(chain, i int) (string, error) {
	if chain < 0 || chain >= len(s.chains) {
		return "", fmt.Errorf("wallet has no chain %d", chain)
	}
	if i < 0 || i >= hdkeychain.HardenedKeyStart || (!s.ranged && i != 0) {
		return "", fmt.Errorf("address index %d out of range", i)
	}
	s.mu.Lock()
	a, ok := s.addrs[[2]int{chain, i}]
	s.mu.Unlock()
	if ok {
		return a, nil
	}
	c := s.chains[chain]
	addr, err := c.d.address(c.path, uint32(i))
	if err != nil {
		return "", err
	}
	a = addr.EncodeAddress()
	s.mu.Lock()
	s.addrs[[2]int{chain, i}] = a
	s.mu.Unlock()
	return a, nil
}
