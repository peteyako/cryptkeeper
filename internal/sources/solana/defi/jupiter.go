package defi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/httpx"
)

// ---- Locked / staked JUP (Jupiter's fork of the Tribeca locked-voter program) ----

// Program, Escrow layout and seeds:
// https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/lib.rs (declare_id!)
// https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/state.rs
// https://github.com/TeamRaccoons/WAGMI/blob/main/programs/locked-voter/src/instructions/new_escrow.rs
// The JUP locker is the PDA ["Locker", base] with base bJ1TRoFo2P6UHVwqdiipp6Qhp2HaaHpLowZ5LHet8Gm;
// on mainnet its token_mint is JUP (checked 2026-10-16). It is an account of the locked-voter
// program, which runs every lock, unlock and withdrawal.
const (
	JupLockedVoterProgram = "voTpe3tHQ7AjQHMapgSue2HJFAh2cGsdokqN3XqmVSj"
	JupLocker             = "CVMdMd79no569tjc5Sq7kzz8isbfCcFyBS5TLGsrZ5dN"
	jupDecimals           = 6
)

var jupEscrowDisc = anchorDiscriminator("Escrow") // 1fd57bbbba16da9b

// Escrow (322 bytes): disc 8 | locker 32 @8 | owner 32 @40 | bump 1 @72 | tokens 32 @73 |
// amount u64 @105 | escrow_started_at i64 @113 | escrow_ends_at i64 @121 | vote_delegate 32 @129 |
// is_max_lock bool @161 | partial_unstaking_amount u64 @162 | padding u64 | buffers [u128; 9].
//
// amount is the actively staked JUP; partial_unstaking_amount is JUP in 7-day partial unstakes
// that is still in the escrow's vault until withdrawn (vault balance = amount + partial).
const (
	escLocker      = 8
	escOwner       = 40
	escAmount      = 105
	escStartedAt   = 113
	escEndsAt      = 121
	escIsMaxLock   = 161
	escPartial     = 162
	escMinLen      = 170
	escMaxDateUnix = 1 << 40 // sanity bound for escrow_ends_at
)

type jupEscrow struct {
	locker, owner     string
	amount, partial   uint64
	startedAt, endsAt int64
	isMaxLock         bool
}

func decodeJupEscrow(data []byte) (jupEscrow, bool) {
	if len(data) < escMinLen || !hasDiscriminator(data, jupEscrowDisc) {
		return jupEscrow{}, false
	}
	return jupEscrow{
		locker:    pubkeyAt(data, escLocker),
		owner:     pubkeyAt(data, escOwner),
		amount:    u64At(data, escAmount),
		startedAt: i64At(data, escStartedAt),
		endsAt:    i64At(data, escEndsAt),
		isMaxLock: data[escIsMaxLock] != 0,
		partial:   u64At(data, escPartial),
	}, true
}

// ---- Jupiter perps ----

// JupiterPerpsAPIBase is the keyless perps API used by Jupiter's own CLI:
// GET /positions?walletAddress=<owner> → {"dataList":[…],"count":n}. USD fields are integers
// with 6 decimals; valueUsd = collateralUsd + pnlAfterFeesUsd.
// https://github.com/jup-ag/cli/blob/main/src/clients/PerpsClient.ts
const JupiterPerpsAPIBase = "https://perps-api.jup.ag/v2"

// JupiterPriceAPIBase is the keyless Price API v3, used for mark prices when the perps API is
// down and positions are decoded from chain instead. https://dev.jup.ag/docs/price/v3
const JupiterPriceAPIBase = "https://lite-api.jup.ag/price/v3"

// Perpetuals program, JLP pool and custodies:
// https://github.com/jup-ag/docs/blob/main/perps/position-account.mdx
// https://github.com/julianfssen/jupiter-perps-anchor-idl-parsing (IDL, constants.ts, PDA seeds)
// Each custody's mint was read from its account on 2026-10-16.
const (
	JupPerpsProgram = "PERPHjGBqRHArX4DySjwM6UJHiR3sWAatqfdBS2qQJu"
	JupPerpsPool    = "5BUwFW4nRbftYTDMbgxykoFWqWHPzahFSNAaaaJtVKsq"
)

type perpsCustody struct{ key, mint, symbol string }

var (
	custodySOL   = perpsCustody{"7xS2gz2bTp3fwCC7knJvUWTEU9Tycczu6VhJYKgi1wdz", wsolMint, "SOL"}
	custodyETH   = perpsCustody{"AQCGyheWPLeo6Qp9WpYS9m3Qj479t7R636N9ey1rEjEn", "7vfCXTUXx5WJV5JADk17DUJ4ksgau7utNKj4b963voxs", "ETH"}
	custodyBTC   = perpsCustody{"5Pv3gM9JrFFH883SWAhvJC9RPYmo8UNxuFtv5bMMALkm", "3NZ9JMVBmGAqocybic2c7LQCJScmgsAZ6vQqTDzcqmJh", "BTC"}
	custodyUSDC  = perpsCustody{"G18jKKXQwBbrHeiK3C9MRXhkHsLHf7XgCSisykV46EZa", usdcMint, "USDC"}
	custodyUSDT  = perpsCustody{"4vkNeXiYEUizLdrpdPS1eC2mccyM4NUPRtERrk6ZETkk", usdtMint, "USDT"}
	perpsMarkets = []perpsCustody{custodySOL, custodyETH, custodyBTC}
	perpsStables = []perpsCustody{custodyUSDC, custodyUSDT}
	custodyByKey = map[string]perpsCustody{
		custodySOL.key: custodySOL, custodyETH.key: custodyETH, custodyBTC.key: custodyBTC,
		custodyUSDC.key: custodyUSDC, custodyUSDT.key: custodyUSDT,
	}
)

var perpsPositionDisc = anchorDiscriminator("Position") // aabc8fe47a40f7d0

// Position (216 bytes on chain): disc 8 | owner 32 @8 | pool 32 @40 | custody 32 @72 |
// collateral_custody 32 @104 | open_time i64 @136 | update_time i64 @144 | side u8 @152
// (1 long, 2 short) | price u64 @153 | size_usd u64 @161 | collateral_usd u64 @169 |
// realised_pnl_usd i64 @177 | cumulative_interest_snapshot u128 @185 | locked_amount u64 @201 | bump.
// PDA seeds: ["position", owner, pool, custody, collateral_custody, [side]].
const (
	perpOwner      = 8
	perpPool       = 40
	perpCustody    = 72
	perpCollateral = 104
	perpSide       = 152
	perpPrice      = 153
	perpSizeUsd    = 161
	perpCollUsd    = 169
	perpMinLen     = 177
	sideLong       = 1
	sideShort      = 2
	usdDecimals    = 6
)

// ---- Adapter ----

type jupiterAdapter struct {
	hc        *httpx.Client
	perpsBase string
	priceBase string
	now       func() time.Time
}

func newJupiter(c *httpx.Client) *jupiterAdapter {
	return &jupiterAdapter{hc: c, perpsBase: JupiterPerpsAPIBase, priceBase: JupiterPriceAPIBase, now: time.Now}
}

func (*jupiterAdapter) Name() string { return NameJupiter }

// Positions reports the owner's locked/staked JUP escrow and open perps positions. JLP is a
// plain SPL token and is not handled here.
func (j *jupiterAdapter) Positions(ctx context.Context, rpc RPC, in Input) ([]Position, error) {
	ownerBytes, err := pubkeyBytes(in.Owner)
	if err != nil {
		return nil, fmt.Errorf("jupiter: owner: %w", err)
	}
	var out []Position
	var errs []error
	if p, err := j.lockedJUP(ctx, rpc, in.Owner, ownerBytes); err != nil {
		errs = append(errs, err)
	} else if p != nil {
		out = append(out, *p)
	}
	perps, err := j.perps(ctx, rpc, in.Owner, ownerBytes)
	if err != nil {
		errs = append(errs, err)
	}
	out = append(out, perps...)
	return out, errors.Join(errs...)
}

func (j *jupiterAdapter) lockedJUP(ctx context.Context, rpc RPC, owner string, ownerBytes []byte) (*Position, error) {
	locker, _ := pubkeyBytes(JupLocker)
	escrowKey, err := findProgramAddress([][]byte{[]byte("Escrow"), locker, ownerBytes}, JupLockedVoterProgram)
	if err != nil {
		return nil, fmt.Errorf("jupiter: escrow address: %w", err)
	}
	accts, err := fetchAccounts(ctx, rpc, []string{escrowKey})
	if err != nil {
		return nil, fmt.Errorf("jupiter: fetch escrow: %w", err)
	}
	data, ok := accts[escrowKey]
	if !ok {
		return nil, nil // never staked
	}
	esc, ok := decodeJupEscrow(data)
	if !ok || esc.locker != JupLocker || esc.owner != owner {
		return nil, fmt.Errorf("jupiter: escrow %s: unexpected account data", escrowKey)
	}
	total := new(big.Int).SetUint64(esc.amount)
	total.Add(total, new(big.Int).SetUint64(esc.partial))
	if total.Sign() == 0 {
		return nil, nil // emptied escrow
	}
	return &Position{
		Protocol: NameJupiter,
		ID:       escrowKey,
		Kind:     KindLocked,
		Label:    j.escrowLabel(esc),
		Legs:     []Leg{legFromRaw(jupMint, total, jupDecimals)},
	}, nil
}

// escrowLabel: max-lock escrows are "staked" (no end date; unstaking is a separate request).
// Otherwise escrow_ends_at is when the escrow unlocks.
func (j *jupiterAdapter) escrowLabel(e jupEscrow) string {
	var label string
	switch {
	case e.isMaxLock || e.endsAt <= 0 || e.endsAt > escMaxDateUnix:
		label = "Staked JUP"
	case time.Unix(e.endsAt, 0).After(j.now()):
		label = "Locked JUP · unlocks " + time.Unix(e.endsAt, 0).UTC().Format("Jan 2, 2006")
	default:
		label = "Locked JUP · unlocked " + time.Unix(e.endsAt, 0).UTC().Format("Jan 2, 2006")
	}
	if e.partial > 0 {
		label += " · " + groupThousands(formatUnits(new(big.Int).SetUint64(e.partial), jupDecimals)) + " unstaking"
	}
	return label
}

// groupThousands inserts commas into the integer part of a decimal string and keeps at most two
// fraction digits: "1998.123456" → "1,998.12".
func groupThousands(s string) string {
	intPart, frac, _ := strings.Cut(s, ".")
	if len(frac) > 2 {
		frac = frac[:2]
	}
	var b strings.Builder
	for i, c := range intPart {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if frac = strings.TrimRight(frac, "0"); frac != "" {
		b.WriteString("." + frac)
	}
	return b.String()
}

// ---- Perps ----

type perpsAPIPosition struct {
	Asset          string `json:"asset"`
	AssetMint      string `json:"assetMint"`
	CollateralMint string `json:"collateralMint"`
	PositionPubkey string `json:"positionPubkey"`
	Side           string `json:"side"`
	Leverage       string `json:"leverage"`
	SizeUsd        string `json:"sizeUsd"`
	CollateralUsd  string `json:"collateralUsd"`
	ValueUsd       string `json:"valueUsd"`
	PnlAfterFees   string `json:"pnlAfterFeesUsd"`
}

type perpsAPIResponse struct {
	DataList []perpsAPIPosition `json:"dataList"`
	Count    int                `json:"count"`
}

// perps uses the perps API; if it fails, positions are decoded from chain and valued at
// collateral + unrealized PnL (before fees) using Jupiter Price API mark prices.
func (j *jupiterAdapter) perps(ctx context.Context, rpc RPC, owner string, ownerBytes []byte) ([]Position, error) {
	out, apiErr := j.perpsFromAPI(ctx, owner)
	if apiErr == nil {
		return out, nil
	}
	out, chainErr := j.perpsFromChain(ctx, rpc, owner, ownerBytes)
	if chainErr != nil {
		return nil, fmt.Errorf("jupiter: perps: API: %v; on-chain fallback: %v", apiErr, chainErr)
	}
	return out, nil
}

func (j *jupiterAdapter) perpsFromAPI(ctx context.Context, owner string) ([]Position, error) {
	var resp perpsAPIResponse
	u := j.perpsBase + "/positions?walletAddress=" + url.QueryEscape(owner)
	if err := j.hc.GetJSON(ctx, u, nil, &resp); err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range resp.DataList {
		value, ok := new(big.Int).SetString(p.ValueUsd, 10)
		if !ok || p.PositionPubkey == "" {
			return nil, fmt.Errorf("position %q: bad valueUsd %q", p.PositionPubkey, p.ValueUsd)
		}
		if size, ok := new(big.Int).SetString(p.SizeUsd, 10); ok && size.Sign() == 0 {
			continue // closed
		}
		label := perpsLabel(p.Side, p.Asset, p.Leverage, false)
		out = append(out, perpsPosition(p.PositionPubkey, label, value))
	}
	return out, nil
}

func perpsLabel(side, asset, leverage string, estimate bool) string {
	s := strings.ToUpper(side[:min(1, len(side))]) + strings.ToLower(side[min(1, len(side)):])
	label := strings.TrimSpace(s + " " + asset)
	if leverage != "" {
		label += " · " + leverage + "x"
	}
	if estimate {
		label += " · est."
	}
	return label
}

// perpsPosition is a perp whose single leg is its USD value (6-decimal integer), expressed as
// USDC so that amount × price agrees with USDValue.
func perpsPosition(id, label string, valueMicroUSD *big.Int) Position {
	leg := legFromRaw(usdcMint, valueMicroUSD, usdDecimals)
	v := leg.Amount
	leg.USDValue = &v
	return Position{Protocol: NameJupiter, ID: id, Kind: KindPerp, Label: label, Legs: []Leg{leg}}
}

type perpsOnChain struct {
	key                 string
	custody, collateral string
	side                byte
	price, size, coll   uint64
}

func decodePerpsPosition(key string, data []byte) (perpsOnChain, bool) {
	if len(data) < perpMinLen || !hasDiscriminator(data, perpsPositionDisc) {
		return perpsOnChain{}, false
	}
	return perpsOnChain{
		key:        key,
		custody:    pubkeyAt(data, perpCustody),
		collateral: pubkeyAt(data, perpCollateral),
		side:       data[perpSide],
		price:      u64At(data, perpPrice),
		size:       u64At(data, perpSizeUsd),
		coll:       u64At(data, perpCollUsd),
	}, true
}

// perpsPDAs derives the nine possible position accounts of an owner: a long per market
// (collateral = the market's own custody) and a short per market per stablecoin.
func perpsPDAs(ownerBytes []byte) ([]string, error) {
	pool, _ := pubkeyBytes(JupPerpsPool)
	derive := func(custody, collateral string, side byte) (string, error) {
		c, _ := pubkeyBytes(custody)
		cc, _ := pubkeyBytes(collateral)
		return findProgramAddress([][]byte{[]byte("position"), ownerBytes, pool, c, cc, {side}}, JupPerpsProgram)
	}
	var keys []string
	for _, m := range perpsMarkets {
		k, err := derive(m.key, m.key, sideLong)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
		for _, s := range perpsStables {
			k, err := derive(m.key, s.key, sideShort)
			if err != nil {
				return nil, err
			}
			keys = append(keys, k)
		}
	}
	return keys, nil
}

func (j *jupiterAdapter) perpsFromChain(ctx context.Context, rpc RPC, owner string, ownerBytes []byte) ([]Position, error) {
	keys, err := perpsPDAs(ownerBytes)
	if err != nil {
		return nil, err
	}
	accts, err := fetchAccounts(ctx, rpc, keys)
	if err != nil {
		return nil, err
	}
	var open []perpsOnChain
	priceMints := map[string]bool{}
	for _, k := range keys {
		data, ok := accts[k]
		if !ok {
			continue
		}
		p, ok := decodePerpsPosition(k, data)
		if !ok || pubkeyAt(data, perpOwner) != owner || pubkeyAt(data, perpPool) != JupPerpsPool {
			return nil, fmt.Errorf("position %s: unexpected account data", k)
		}
		if p.size == 0 {
			continue // closed positions keep their account
		}
		c, ok := custodyByKey[p.custody]
		if !ok || p.price == 0 || (p.side != sideLong && p.side != sideShort) {
			return nil, fmt.Errorf("position %s: unexpected custody/side/price", k)
		}
		open = append(open, p)
		priceMints[c.mint] = true
	}
	if len(open) == 0 {
		return nil, nil
	}
	prices, err := j.markPrices(ctx, priceMints)
	if err != nil {
		return nil, err
	}
	var out []Position
	for _, p := range open {
		c := custodyByKey[p.custody]
		mark, ok := prices[c.mint]
		if !ok {
			return nil, fmt.Errorf("no mark price for %s", c.symbol)
		}
		entry := float64(p.price) / 1e6
		size := float64(p.size)
		pnl := size * (mark - entry) / entry
		side := "long"
		if p.side == sideShort {
			pnl = -pnl
			side = "short"
		}
		value := float64(p.coll) + pnl // micro-USD
		if math.IsNaN(value) || math.Abs(value) > 1e18 {
			return nil, fmt.Errorf("position %s: implausible value", p.key)
		}
		lev := ""
		if p.coll > 0 {
			lev = strconv.FormatFloat(size/float64(p.coll), 'f', 2, 64)
		}
		out = append(out, perpsPosition(p.key, perpsLabel(side, c.symbol, lev, true), big.NewInt(int64(math.Round(value)))))
	}
	return out, nil
}

type jupPrice struct {
	USDPrice float64 `json:"usdPrice"`
}

func (j *jupiterAdapter) markPrices(ctx context.Context, mints map[string]bool) (map[string]float64, error) {
	ids := make([]string, 0, len(mints))
	for _, m := range perpsMarkets { // fixed order keeps the request deterministic
		if mints[m.mint] {
			ids = append(ids, m.mint)
		}
	}
	var resp map[string]json.RawMessage
	if err := j.hc.GetJSON(ctx, j.priceBase+"?ids="+strings.Join(ids, ","), nil, &resp); err != nil {
		return nil, fmt.Errorf("mark prices: %w", err)
	}
	out := map[string]float64{}
	for _, id := range ids {
		raw, ok := resp[id]
		if !ok || string(raw) == "null" {
			continue
		}
		var p jupPrice
		if err := json.Unmarshal(raw, &p); err != nil || p.USDPrice <= 0 || math.IsInf(p.USDPrice, 0) || math.IsNaN(p.USDPrice) {
			continue
		}
		out[id] = p.USDPrice
	}
	return out, nil
}
