package bitcoin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/btcsuite/btcd/btcec/v2"
	"github.com/btcsuite/btcd/btcec/v2/schnorr"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/btcsuite/btcd/txscript"
)

// Output descriptors (BIP380-386, BIP389 multipath). Supported:
//
//	pkh(KEY) wpkh(KEY) sh(wpkh(KEY)) tr(KEY)
//	wsh(MULTI) sh(wsh(MULTI)) sh(MULTI)        MULTI = multi(k,KEY,...) | sortedmulti(k,KEY,...)
//	addr(ADDRESS)
//
// Bare multi()/sortedmulti() parse (for script tests) but have no address.

// descKind is the output script shape of a parsed descriptor.
type descKind int

const (
	kindPKH descKind = iota + 1
	kindSHWPKH
	kindWPKH
	kindTR
	kindSHMulti
	kindWSHMulti
	kindSHWSHMulti
	kindBareMulti
	kindAddr
)

const (
	maxP2SHMultiKeys  = 15  // BIP383
	maxMultiKeys      = 20  // OP_CHECKMULTISIG limit
	maxRedeemScript   = 520 // P2SH redeem script size limit
	compressedKeySize = 33
)

// descriptor is a parsed output descriptor (or an xpub turned into one).
type descriptor struct {
	kind      descKind
	keys      []*keyExpr
	threshold int  // multisig only
	sorted    bool // sortedmulti (BIP67)
	addr      btcutil.Address
	npaths    int // number of multipath alternatives; 1 when there is no <a;b> step
}

// keyExpr is one KEY expression: an optional origin, then a fixed public key or an
// extended public key with non-hardened derivation steps and an optional /* wildcard.
type keyExpr struct {
	fingerprint []byte   // origin fingerprint; nil when no origin was given
	origin      []uint32 // origin path, informational only

	ser []byte           // fixed key serialization (hex keys)
	pub *btcec.PublicKey // fixed key

	ext      *hdkeychain.ExtendedKey
	steps    [][]uint32 // derivation after ext; a multipath step has more than one element
	wildcard bool

	mu      sync.Mutex
	parents map[int]*hdkeychain.ExtendedKey // ext derived through steps, per multipath index
}

// ---- BIP380 checksum ----

const (
	descInputCharset    = "0123456789()[],'/*abcdefgh@:$%{}IJKLMNOPQRSTUVWXYZ&+-.;<=>?!^_|~ijklmnopqrstuvwxyzABCDEFGH`#\"\\ "
	descChecksumCharset = "qpzry9x8gf2tvdw0s3jn54khce6mua7l"
)

var descGenerator = [5]uint64{0xf5dee51989, 0xa9fdca3312, 0x1bab10e32d, 0x3706b1677a, 0x644d626ffd}

func descPolymod(c uint64, val int) uint64 {
	top := c >> 35
	c = (c&0x7ffffffff)<<5 ^ uint64(val)
	for i, g := range descGenerator {
		if (top>>i)&1 != 0 {
			c ^= g
		}
	}
	return c
}

// descChecksum computes the 8-character BIP380 checksum of a descriptor body.
func descChecksum(s string) (string, error) {
	c := uint64(1)
	cls, clsCount := 0, 0
	for _, ch := range s {
		pos := strings.IndexRune(descInputCharset, ch)
		if pos < 0 {
			return "", fmt.Errorf("invalid character %q in descriptor", ch)
		}
		c = descPolymod(c, pos&31)
		cls = cls*3 + pos>>5
		if clsCount++; clsCount == 3 {
			c = descPolymod(c, cls)
			cls, clsCount = 0, 0
		}
	}
	if clsCount > 0 {
		c = descPolymod(c, cls)
	}
	for range 8 {
		c = descPolymod(c, 0)
	}
	c ^= 1
	out := make([]byte, 8)
	for j := range out {
		out[j] = descChecksumCharset[(c>>(5*(7-j)))&31]
	}
	return string(out), nil
}

// stripChecksum verifies the "#checksum" suffix when present and returns the body.
func stripChecksum(s string) (string, error) {
	switch strings.Count(s, "#") {
	case 0:
		return s, nil
	case 1:
	default:
		return "", errors.New("descriptor has more than one '#' (malformed checksum)")
	}
	body, sum, _ := strings.Cut(s, "#")
	if len(sum) != 8 {
		return "", fmt.Errorf("descriptor checksum must be 8 characters, got %d", len(sum))
	}
	want, err := descChecksum(body)
	if err != nil {
		return "", err
	}
	if sum != want {
		return "", errors.New("descriptor checksum does not match (the descriptor was altered or mistyped)")
	}
	return body, nil
}

// ---- parsing ----

// parseDescriptor parses one descriptor, verifying its checksum when present.
func parseDescriptor(s string) (*descriptor, error) {
	body, err := stripChecksum(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if body == "" {
		return nil, errors.New("empty descriptor")
	}
	d, err := parseTop(body)
	if err != nil {
		return nil, err
	}
	d.npaths = 1
	for _, k := range d.keys {
		if n := k.npaths(); n > 1 {
			if d.npaths > 1 && n != d.npaths {
				return nil, errors.New("multipath steps <…> in the descriptor have different lengths")
			}
			d.npaths = n
		}
	}
	return d, nil
}

// splitCall splits "name(args)" when the parenthesis opened after name closes at the end.
func splitCall(s string) (name, args string, ok bool) {
	open := strings.IndexByte(s, '(')
	if open <= 0 || !strings.HasSuffix(s, ")") {
		return "", "", false
	}
	depth := 0
	for i := open; i < len(s); i++ {
		switch s[i] {
		case '(':
			depth++
		case ')':
			if depth--; depth == 0 && i != len(s)-1 {
				return "", "", false
			}
		}
	}
	if depth != 0 {
		return "", "", false
	}
	return s[:open], s[open+1 : len(s)-1], true
}

func unsupported(name string) error {
	return fmt.Errorf("unsupported descriptor %s(): Vault supports pkh, wpkh, sh(wpkh), tr(KEY), "+
		"wsh/sh(wsh)/sh of multi or sortedmulti, and addr", name)
}

type keyCtx struct {
	uncompressedOK bool // legacy contexts (pkh, sh(multi), bare multi)
	xonlyOK        bool // tr()
}

func parseTop(s string) (*descriptor, error) {
	name, args, ok := splitCall(s)
	if !ok {
		return nil, errors.New("not an output descriptor: expected SCRIPT(...), e.g. wpkh([fingerprint/84h/0h/0h]xpub…/<0;1>/*)")
	}
	switch name {
	case "pkh":
		return singleKey(kindPKH, args, keyCtx{uncompressedOK: true})
	case "wpkh":
		return singleKey(kindWPKH, args, keyCtx{})
	case "tr":
		if strings.Contains(args, ",") {
			return nil, errors.New("tr() script trees are not supported: only key-path tr(KEY) descriptors can be watched")
		}
		return singleKey(kindTR, args, keyCtx{xonlyOK: true})
	case "sh":
		return parseSH(args)
	case "wsh":
		return parseWSH(args)
	case "multi", "sortedmulti":
		return parseMulti(kindBareMulti, name, args)
	case "addr":
		a, err := parseAddress(args)
		if err != nil {
			return nil, err
		}
		return &descriptor{kind: kindAddr, addr: a}, nil
	}
	return nil, unsupported(name)
}

func singleKey(kind descKind, args string, ctx keyCtx) (*descriptor, error) {
	k, err := parseKey(args, ctx)
	if err != nil {
		return nil, err
	}
	return &descriptor{kind: kind, keys: []*keyExpr{k}}, nil
}

func parseSH(args string) (*descriptor, error) {
	name, inner, ok := splitCall(args)
	if !ok {
		return nil, errors.New("sh() takes a script: sh(wpkh(KEY)), sh(wsh(multi(…))) or sh(multi(…))")
	}
	switch name {
	case "wpkh":
		return singleKey(kindSHWPKH, inner, keyCtx{})
	case "wsh":
		d, err := parseWSH(inner)
		if err != nil {
			return nil, err
		}
		d.kind = kindSHWSHMulti
		return d, nil
	case "multi", "sortedmulti":
		return parseMulti(kindSHMulti, name, inner)
	case "sh":
		return nil, errors.New("sh() cannot be nested inside sh()")
	case "tr":
		return nil, errors.New("tr() cannot be nested inside sh()")
	}
	return nil, unsupported("sh(" + name + ")")
}

func parseWSH(args string) (*descriptor, error) {
	name, inner, ok := splitCall(args)
	if !ok {
		return nil, errors.New("wsh() takes a script: wsh(multi(…)) or wsh(sortedmulti(…))")
	}
	switch name {
	case "multi", "sortedmulti":
		return parseMulti(kindWSHMulti, name, inner)
	case "wsh", "wpkh", "sh", "tr":
		return nil, fmt.Errorf("%s() cannot be nested inside wsh()", name)
	}
	return nil, unsupported("wsh(" + name + ")")
}

func parseMulti(kind descKind, name, args string) (*descriptor, error) {
	parts := strings.Split(args, ",")
	if len(parts) < 2 {
		return nil, fmt.Errorf("%s() needs a threshold and at least one key", name)
	}
	if !isDigits(parts[0]) {
		return nil, fmt.Errorf("%s() threshold %q is not a number", name, parts[0])
	}
	threshold, _ := strconv.Atoi(parts[0])
	keys := parts[1:]
	if threshold < 1 || threshold > len(keys) {
		return nil, fmt.Errorf("%s() threshold %d must be between 1 and the number of keys (%d)", name, threshold, len(keys))
	}
	limit := maxMultiKeys
	if kind == kindSHMulti {
		limit = maxP2SHMultiKeys
	}
	if len(keys) > limit {
		return nil, fmt.Errorf("%s() has %d keys; at most %d are allowed here", name, len(keys), limit)
	}
	ctx := keyCtx{uncompressedOK: kind == kindSHMulti || kind == kindBareMulti}
	d := &descriptor{kind: kind, threshold: threshold, sorted: name == "sortedmulti"}
	scriptSize := 3 // OP_k, OP_n, OP_CHECKMULTISIG
	for i, ks := range keys {
		k, err := parseKey(ks, ctx)
		if err != nil {
			return nil, fmt.Errorf("%s() key #%d: %w", name, i+1, err)
		}
		size := compressedKeySize
		if k.ext == nil {
			size = len(k.ser)
		}
		scriptSize += 1 + size
		d.keys = append(d.keys, k)
	}
	if kind == kindSHMulti && scriptSize > maxRedeemScript {
		return nil, fmt.Errorf("P2SH multisig script is %d bytes, larger than the %d byte limit", scriptSize, maxRedeemScript)
	}
	return d, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}

// parseIndex parses one BIP32 path element. Hardened markers h, H and ' are recognised
// (BIP380 lists H as invalid, but some wallets emit it).
func parseIndex(p string) (idx uint32, hardened bool, err error) {
	if p == "" {
		return 0, false, errors.New("empty derivation index")
	}
	if c := p[len(p)-1]; c == 'h' || c == 'H' || c == '\'' {
		hardened, p = true, p[:len(p)-1]
	}
	if !isDigits(p) {
		return 0, false, fmt.Errorf("invalid derivation step %q", p)
	}
	n, err := strconv.ParseUint(p, 10, 32)
	if err != nil || n >= hdkeychain.HardenedKeyStart {
		return 0, false, fmt.Errorf("derivation index %s is out of range", p)
	}
	return uint32(n), hardened, nil
}

func errHardened(step string) error {
	return fmt.Errorf("hardened derivation step %q needs the private key: use the account-level xpub "+
		"with unhardened steps such as /<0;1>/*", step)
}

func parseKey(s string, ctx keyCtx) (*keyExpr, error) {
	k := &keyExpr{}
	if strings.HasPrefix(s, "[") {
		end := strings.IndexByte(s, ']')
		if end < 0 {
			return nil, errors.New("key origin is missing its closing ']'")
		}
		if err := k.parseOrigin(s[1:end]); err != nil {
			return nil, err
		}
		s = s[end+1:]
	}
	switch {
	case strings.ContainsAny(s, "[]"):
		return nil, errors.New("misplaced '[' or ']': a key may have one origin [fingerprint/path] before it")
	case s == "":
		return nil, errors.New("missing key")
	case strings.ContainsAny(s, "(),"):
		return nil, errors.New("expected a key, found a script expression")
	}
	parts := strings.Split(s, "/")
	material, steps := parts[0], parts[1:]
	switch {
	case isHex(material):
		if len(steps) > 0 {
			return nil, errors.New("a hex public key cannot have derivation steps")
		}
		return k, k.parseHexKey(material, ctx)
	case isWIF(material):
		return nil, errPrivateKey
	case looksLikeExtendedKey(material):
		ext, _, err := parseExtendedPub(material)
		if err != nil {
			return nil, err
		}
		k.ext = ext
		return k, k.parseSteps(steps)
	}
	return nil, fmt.Errorf("unrecognised key %q: expected an xpub or a hex public key", redact(material))
}

func (k *keyExpr) parseOrigin(o string) error {
	parts := strings.Split(o, "/")
	fp := parts[0]
	if len(fp) != 8 {
		return fmt.Errorf("key origin fingerprint must be 8 hex characters, got %d", len(fp))
	}
	b, err := hex.DecodeString(fp)
	if err != nil {
		return fmt.Errorf("key origin fingerprint %q is not hex", fp)
	}
	k.fingerprint = b
	for _, p := range parts[1:] {
		idx, hardened, err := parseIndex(p)
		if err != nil {
			return fmt.Errorf("key origin: %w", err)
		}
		if hardened {
			idx += hdkeychain.HardenedKeyStart
		}
		k.origin = append(k.origin, idx)
	}
	return nil
}

func (k *keyExpr) parseHexKey(h string, ctx keyCtx) error {
	b, err := hex.DecodeString(h)
	if err != nil {
		return fmt.Errorf("hex public key: %w", err)
	}
	switch len(b) {
	case 32:
		if !ctx.xonlyOK {
			return errors.New("x-only (32-byte) public keys are only allowed in tr()")
		}
		pub, err := schnorr.ParsePubKey(b)
		if err != nil {
			return fmt.Errorf("x-only public key: %w", err)
		}
		k.pub, k.ser = pub, pub.SerializeCompressed()
		return nil
	case 33:
	case 65:
		if !ctx.uncompressedOK {
			return errors.New("uncompressed public keys are not allowed in segwit or taproot scripts")
		}
	default:
		return fmt.Errorf("hex public key must be 33 bytes (compressed) or 65 bytes (uncompressed), got %d", len(b))
	}
	pub, err := btcec.ParsePubKey(b)
	if err != nil {
		return fmt.Errorf("hex public key: %w", err)
	}
	k.pub, k.ser = pub, b
	return nil
}

func (k *keyExpr) parseSteps(steps []string) error {
	multipath := false
	for i, st := range steps {
		switch {
		case st == "*":
			if i != len(steps)-1 {
				return errors.New("'*' must be the last derivation step")
			}
			k.wildcard = true
		case strings.HasPrefix(st, "*"):
			return errHardened(st)
		case strings.HasPrefix(st, "<"):
			if !strings.HasSuffix(st, ">") {
				return fmt.Errorf("multipath step %q is missing its closing '>'", st)
			}
			if multipath {
				return errors.New("only one <a;b> multipath step is allowed per key")
			}
			multipath = true
			alts := strings.Split(st[1:len(st)-1], ";")
			if len(alts) < 2 {
				return fmt.Errorf("multipath step %q needs at least two indexes", st)
			}
			vals := make([]uint32, len(alts))
			for j, a := range alts {
				idx, hardened, err := parseIndex(a)
				if err != nil {
					return fmt.Errorf("multipath step %q: %w", st, err)
				}
				if hardened {
					return errHardened(st)
				}
				vals[j] = idx
			}
			k.steps = append(k.steps, vals)
		default:
			if strings.ContainsAny(st, "<>;") {
				return fmt.Errorf("malformed multipath step %q", st)
			}
			idx, hardened, err := parseIndex(st)
			if err != nil {
				return err
			}
			if hardened {
				return errHardened(st)
			}
			k.steps = append(k.steps, []uint32{idx})
		}
	}
	return nil
}

// parseAddress decodes a mainnet address; other networks get a "mainnet only" error.
func parseAddress(s string) (btcutil.Address, error) {
	if a, err := btcutil.DecodeAddress(s, mainnet); err == nil && a.IsForNet(mainnet) {
		return a, nil
	}
	for _, net := range []*chaincfg.Params{&chaincfg.TestNet3Params, &chaincfg.RegressionNetParams, &chaincfg.SigNetParams} {
		if a, err := btcutil.DecodeAddress(s, net); err == nil && a.IsForNet(net) {
			return nil, errTestnet("address " + s)
		}
	}
	return nil, fmt.Errorf("%q is not a valid Bitcoin address", redact(s))
}

// ---- derivation ----

func (k *keyExpr) npaths() int {
	for _, s := range k.steps {
		if len(s) > 1 {
			return len(s)
		}
	}
	return 1
}

// parent returns ext derived through every step for multipath alternative path. Cached,
// because deriving is an EC multiplication per step and the scan calls it per address.
func (k *keyExpr) parent(path int) (*hdkeychain.ExtendedKey, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if p, ok := k.parents[path]; ok {
		return p, nil
	}
	cur := k.ext
	for _, alts := range k.steps {
		i := alts[0]
		if len(alts) > 1 {
			i = alts[path]
		}
		var err error
		if cur, err = cur.Derive(i); err != nil {
			return nil, fmt.Errorf("derive child %d: %w", i, err)
		}
	}
	if k.parents == nil {
		k.parents = map[int]*hdkeychain.ExtendedKey{}
	}
	k.parents[path] = cur
	return cur, nil
}

// derive returns the key at (path, index): its serialization and parsed form.
func (k *keyExpr) derive(path int, index uint32) ([]byte, *btcec.PublicKey, error) {
	if k.ext == nil {
		return k.ser, k.pub, nil
	}
	child, err := k.parent(path)
	if err != nil {
		return nil, nil, err
	}
	if k.wildcard {
		if child, err = child.Derive(index); err != nil {
			return nil, nil, fmt.Errorf("derive child %d: %w", index, err)
		}
	}
	pub, err := child.ECPubKey()
	if err != nil {
		return nil, nil, err
	}
	return pub.SerializeCompressed(), pub, nil
}

func (d *descriptor) ranged() bool {
	for _, k := range d.keys {
		if k.wildcard {
			return true
		}
	}
	return false
}

func (d *descriptor) singleScript() scriptType {
	switch d.kind {
	case kindPKH:
		return scriptP2PKH
	case kindSHWPKH:
		return scriptP2SHP2WPKH
	case kindWPKH:
		return scriptP2WPKH
	case kindTR:
		return scriptP2TR
	}
	return 0
}

// multisigScript builds OP_k <pubkeys> OP_n OP_CHECKMULTISIG at (path, index); for
// sortedmulti the derived keys are sorted lexicographically (BIP67).
func (d *descriptor) multisigScript(path int, index uint32) ([]byte, error) {
	keys := make([][]byte, len(d.keys))
	for i, k := range d.keys {
		ser, _, err := k.derive(path, index)
		if err != nil {
			return nil, err
		}
		keys[i] = ser
	}
	if d.sorted {
		sort.Slice(keys, func(a, b int) bool { return bytes.Compare(keys[a], keys[b]) < 0 })
	}
	b := txscript.NewScriptBuilder().AddInt64(int64(d.threshold))
	for _, k := range keys {
		b.AddData(k)
	}
	return b.AddInt64(int64(len(keys))).AddOp(txscript.OP_CHECKMULTISIG).Script()
}

// address returns the mainnet address at (path, index).
func (d *descriptor) address(path int, index uint32) (btcutil.Address, error) {
	if path < 0 || path >= max(d.npaths, 1) {
		return nil, fmt.Errorf("descriptor has no derivation path #%d", path)
	}
	if st := d.singleScript(); st != 0 {
		ser, pub, err := d.keys[0].derive(path, index)
		if err != nil {
			return nil, err
		}
		return addressForKey(st, ser, pub, mainnet)
	}
	switch d.kind {
	case kindAddr:
		return d.addr, nil
	case kindBareMulti:
		return nil, errors.New("bare multisig descriptors have no address: wrap it in wsh() or sh()")
	}
	script, err := d.multisigScript(path, index)
	if err != nil {
		return nil, err
	}
	switch d.kind {
	case kindSHMulti:
		return btcutil.NewAddressScriptHash(script, mainnet)
	case kindWSHMulti:
		sum := sha256.Sum256(script)
		return btcutil.NewAddressWitnessScriptHash(sum[:], mainnet)
	case kindSHWSHMulti:
		sum := sha256.Sum256(script)
		return btcutil.NewAddressScriptHash(append([]byte{txscript.OP_0, txscript.OP_DATA_32}, sum[:]...), mainnet)
	}
	return nil, fmt.Errorf("descriptor kind %d has no address", d.kind)
}

// scriptPubKey returns the output script at (path, index).
func (d *descriptor) scriptPubKey(path int, index uint32) ([]byte, error) {
	if d.kind == kindBareMulti {
		return d.multisigScript(path, index)
	}
	a, err := d.address(path, index)
	if err != nil {
		return nil, err
	}
	return txscript.PayToAddrScript(a)
}

// inferChange turns a receive-only descriptor whose ranged keys all end in /0/* into a
// receive/change pair (/<0;1>/*), the standard BIP44 layout. Reports whether it did.
func (d *descriptor) inferChange() bool {
	if d.npaths != 1 {
		return false
	}
	var ranged []*keyExpr
	for _, k := range d.keys {
		if !k.wildcard {
			continue
		}
		if len(k.steps) == 0 {
			return false
		}
		if last := k.steps[len(k.steps)-1]; len(last) != 1 || last[0] != 0 {
			return false
		}
		ranged = append(ranged, k)
	}
	if len(ranged) == 0 {
		return false
	}
	for _, k := range ranged {
		k.steps[len(k.steps)-1] = []uint32{0, 1}
	}
	d.npaths = 2
	return true
}

// ---- display ----

// label is the script description shown in the wallet's Connection panel.
func (d *descriptor) label() string {
	if st := d.singleScript(); st != 0 {
		return st.label()
	}
	m := fmt.Sprintf("%d-of-%d multisig", d.threshold, len(d.keys))
	switch d.kind {
	case kindSHMulti:
		return m + " (p2sh)"
	case kindWSHMulti:
		return m + " (p2wsh)"
	case kindSHWSHMulti:
		return m + " (p2sh-p2wsh)"
	case kindBareMulti:
		return m + " (bare)"
	case kindAddr:
		return addressLabel(d.addr)
	}
	return "unknown"
}

func addressLabel(a btcutil.Address) string {
	switch a.(type) {
	case *btcutil.AddressPubKeyHash:
		return scriptP2PKH.label()
	case *btcutil.AddressScriptHash:
		return "Script hash (p2sh)"
	case *btcutil.AddressWitnessPubKeyHash:
		return scriptP2WPKH.label()
	case *btcutil.AddressWitnessScriptHash:
		return "SegWit script (p2wsh)"
	case *btcutil.AddressTaproot:
		return scriptP2TR.label()
	}
	return "Address"
}

// derivation returns the first key origin path, e.g. "m/84'/0'/0'", or "" if none is known.
func (d *descriptor) derivation() string {
	for _, k := range d.keys {
		if len(k.origin) > 0 {
			return formatPath(k.origin)
		}
	}
	return ""
}

func formatPath(path []uint32) string {
	var b strings.Builder
	b.WriteString("m")
	for _, p := range path {
		if p >= hdkeychain.HardenedKeyStart {
			fmt.Fprintf(&b, "/%d'", p-hdkeychain.HardenedKeyStart)
		} else {
			fmt.Fprintf(&b, "/%d", p)
		}
	}
	return b.String()
}

// descriptorFromXpub builds the receive/change descriptor for a single-key HD account:
// SCRIPT(xpub/<0;1>/*).
func descriptorFromXpub(k *hdkeychain.ExtendedKey, st scriptType) *descriptor {
	kinds := map[scriptType]descKind{
		scriptP2PKH: kindPKH, scriptP2SHP2WPKH: kindSHWPKH, scriptP2WPKH: kindWPKH, scriptP2TR: kindTR,
	}
	return &descriptor{
		kind:   kinds[st],
		keys:   []*keyExpr{{ext: k, steps: [][]uint32{{0, 1}}, wildcard: true}},
		npaths: 2,
	}
}
