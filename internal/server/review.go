package server

import (
	"context"
	"sort"
	"strconv"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/tax"
	"cryptkeeper/web/templates"
)

// reviewCap is how many items a review group lists before "+N more".
const reviewCap = 50

func tagStrings(tags []tax.Tag) []string {
	out := make([]string, len(tags))
	for i, t := range tags {
		out[i] = string(t)
	}
	return out
}

// reviewTitles names each issue kind's group, in display order.
var reviewTitles = []struct {
	kind  tax.IssueKind
	title string
}{
	{tax.IssueUnmatchedReceive, "Unmatched receives"},
	{tax.IssueUnmatchedSend, "Unmatched sends"},
	{tax.IssueMissingPrice, "Missing prices"},
	{tax.IssueMissingBasis, "Missing cost basis"},
	{tax.IssuePositionExcess, "Position returned more than went in"},
	{tax.IssueTransferUnpaired, "Unpaired transfers"},
	{tax.IssueWriteOffExceedsParked, "Write-off exceeds parked"},
	{tax.IssueUndatedLot, "Manual lots without a date"},
}

// reviewTagOptions are the tags a user may pick for tx: the receive set for "in", the send
// set for "out". Kinds other than receive, send, other and position get none: the engine
// ignores tags on them. A tx with a PeerID gets them only when unpaired (its peer is missing:
// tax.Result.Unpaired), whatever its kind, as the engine then honours its tag. position-gain
// is offered on a receive that is a position move (a position tx, or one tagged position).
func reviewTagOptions(tx core.Tx, direction string, current tax.Tag, unpaired bool) []string {
	switch {
	case tx.PeerID != "":
		if !unpaired {
			return nil
		}
	case tx.Kind == core.TxReceive, tx.Kind == core.TxSend, tx.Kind == core.TxOther, tx.Kind == core.TxPosition:
	default:
		return nil
	}
	switch direction {
	case "in":
		opts := tagStrings(tax.ReceiveTags())
		if tx.Kind == core.TxPosition || current == tax.TagPosition {
			opts = append(opts, string(tax.TagPositionGain))
		}
		return opts
	case "out":
		return tagStrings(tax.SendTags())
	}
	return nil
}

// assetDirection is "in" or "out" from the sign of tx's leg for asset (or its fee), "" when
// tx has neither.
func assetDirection(tx core.Tx, asset string) string {
	for _, l := range tx.Legs {
		if l.AssetKey == asset {
			if l.Amount > 0 {
				return "in"
			}
			return "out"
		}
	}
	if tx.Fee != nil && tx.Fee.AssetKey == asset {
		return "out"
	}
	return ""
}

// writeOffAt and writeOffAmount identify a write-off to its delete form: exact text forms
// of its date and amount, compared as strings.
func writeOffAt(w tax.WriteOff) string     { return w.Date.UTC().Format(time.RFC3339Nano) }
func writeOffAmount(w tax.WriteOff) string { return strconv.FormatFloat(w.Amount, 'g', -1, 64) }

// symbolNames resolves asset keys to symbols: the latest snapshot first, then the legs of
// activity seen, then the short form of a Solana mint.
type symbolNames map[string]string

func (m symbolNames) of(asset string) string {
	if s := m[asset]; s != "" {
		return s
	}
	if len(asset) > 4 && asset[:4] == "spl:" {
		mint := asset[4:]
		if len(mint) > 8 {
			return mint[:4] + "…" + mint[len(mint)-4:]
		}
		return mint
	}
	return asset
}

// reviewPage builds the "Needs review" panel from the open year's tax result.
func (s *Server) reviewPage(ctx context.Context) templates.ReviewPage {
	loc := s.taxLoc()
	p := templates.ReviewPage{}
	var settings tax.Settings
	if s.TaxSettings != nil {
		settings = s.TaxSettings.Get()
		for _, l := range settings.ManualLots {
			if l.Acquired.IsZero() {
				p.UndatedLots++
			}
		}
		// Once a year is closed, a dated manual lot before the open year belongs to a closed
		// year and is ignored too.
		if s.TaxYears != nil {
			if last, ok := s.TaxYears.LastClosed(); ok {
				start := tax.OpenYearStart(last, ok, loc)
				for _, l := range settings.ManualLots {
					if !l.Acquired.IsZero() && l.Acquired.Before(start) {
						p.EarlyLots++
					}
				}
				p.OpenStart = start.Format("Jan 2, 2006")
			}
		}
	}
	labels, _ := s.sourceLabels()
	label := func(w string) string {
		switch {
		case w == tax.Untracked:
			return "Untracked"
		case labels[w] != "":
			return labels[w]
		}
		return w
	}
	names := symbolNames{}
	snap, _ := s.latest()
	for _, a := range snap.Assets {
		names[a.AssetKey] = a.Symbol
	}
	txs := map[string]core.Tx{}
	earliest := 0
	if s.Activity != nil {
		if all, err := s.allActivity(ctx); err == nil {
			p.TagsChecked = true
			for _, tx := range all {
				if y := tx.Time.In(loc).Year(); earliest == 0 || y < earliest {
					earliest = y
				}
				txs[tx.ID] = tx
				for _, l := range tx.Legs {
					if names[l.AssetKey] == "" && l.Symbol != "" {
						names[l.AssetKey] = l.Symbol
					}
				}
			}
		}
	}

	var openOK bool
	p.OpenYear, openOK = s.openYear(earliest)
	if !openOK {
		p.OpenYear = s.Now().In(loc).Year()
	}
	p.Close = s.idleCloseCard(p.OpenYear, openOK)

	res, resOK := s.taxResult(ctx)
	unpaired := make(map[string]bool, len(res.Unpaired))
	for _, id := range res.Unpaired {
		unpaired[id] = true
	}

	item := func(id, wallet, asset string, amount float64, at time.Time, kind tax.IssueKind) templates.ReviewItem {
		it := templates.ReviewItem{
			TxID: id, WalletLabel: label(wallet), Time: at.In(loc).Format("2006-01-02 15:04"),
			Asset: asset, Symbol: names.of(asset), Amount: templates.CoinAmount(amount, names.of(asset)),
			Tag: string(settings.Tags[id].Tag), Note: settings.Tags[id].Note,
		}
		tx, ok := txs[id]
		if !ok {
			return it
		}
		it.Explorer = templates.SafeExplorerURL(tx.ExplorerURL)
		leg := -1
		for i, l := range tx.Legs {
			if l.AssetKey == asset {
				leg = i
				break
			}
		}
		it.Direction = assetDirection(tx, asset)
		if leg >= 0 {
			if u := tx.Legs[leg].USD; u != nil {
				v := *u
				if v < 0 {
					v = -v
				}
				it.USD = &v
			}
		}
		if kind == tax.IssueMissingPrice {
			switch {
			case leg >= 0:
				it.PriceKey = tax.PriceKey(id, leg)
			case tx.Fee != nil && tx.Fee.AssetKey == asset:
				it.PriceKey = tax.FeePriceKey(id)
			}
		}
		it.TagOptions = reviewTagOptions(tx, it.Direction, settings.Tags[id].Tag, unpaired[id])
		return it
	}

	if resOK {
		type gkey struct {
			kind  tax.IssueKind
			asset string
		}
		byKey := map[gkey][]tax.Issue{}
		for _, is := range res.Issues {
			k := gkey{is.Kind, is.Asset}
			byKey[k] = append(byKey[k], is)
		}
		for _, kt := range reviewTitles {
			var groups []templates.ReviewGroup
			for k, issues := range byKey {
				if k.kind != kt.kind {
					continue
				}
				sort.SliceStable(issues, func(i, j int) bool { return issues[i].Time.After(issues[j].Time) })
				g := templates.ReviewGroup{Kind: kt.kind, Title: kt.title, Asset: k.asset, Symbol: names.of(k.asset), Count: len(issues)}
				for _, is := range issues[:min(len(issues), reviewCap)] {
					g.Items = append(g.Items, item(is.TxID, is.Wallet, is.Asset, is.Amount, is.Time, is.Kind))
				}
				groups = append(groups, g)
			}
			sort.Slice(groups, func(i, j int) bool {
				if groups[i].Count != groups[j].Count {
					return groups[i].Count > groups[j].Count
				}
				return groups[i].Asset < groups[j].Asset
			})
			p.Groups = append(p.Groups, groups...)
		}
		for _, pk := range res.Parked {
			p.Parked = append(p.Parked, templates.ParkedRow{
				Wallet: pk.Wallet, WalletLabel: label(pk.Wallet), Asset: pk.Asset, Symbol: names.of(pk.Asset), Amount: pk.Amount,
			})
		}
	}

	for i, w := range settings.WriteOffs {
		p.WriteOffs = append(p.WriteOffs, templates.WriteOffRow{
			Index: i, Wallet: w.Wallet, Asset: w.Asset, WalletLabel: label(w.Wallet), Symbol: names.of(w.Asset),
			Amount: w.Amount, Date: w.Date.In(loc).Format("2006-01-02"), ProceedsUSD: w.ProceedsUSD, Note: w.Note,
			At: writeOffAt(w), AmountKey: writeOffAmount(w),
		})
	}

	// Tagged transactions stay reachable so a tag can be changed; tags whose transaction
	// is gone are listed apart. Without the activity nothing can be told apart, so skip both.
	if !p.TagsChecked {
		return p
	}
	tagged := templates.ReviewGroup{Kind: templates.ReviewTagged, Title: "Tagged"}
	type tagRow struct {
		tx core.Tx
		id string
	}
	var rows []tagRow
	for id, e := range settings.Tags {
		tx, ok := txs[id]
		if !ok {
			p.Stale = append(p.Stale, templates.StaleTag{TxID: id, Tag: e.Tag})
			continue
		}
		rows = append(rows, tagRow{tx, id})
	}
	sort.Slice(p.Stale, func(i, j int) bool { return p.Stale[i].TxID < p.Stale[j].TxID })
	sort.Slice(rows, func(i, j int) bool {
		if !rows[i].tx.Time.Equal(rows[j].tx.Time) {
			return rows[i].tx.Time.After(rows[j].tx.Time)
		}
		return rows[i].id < rows[j].id
	})
	tagged.Count = len(rows)
	for _, r := range rows[:min(len(rows), reviewCap)] {
		// The first leg stands for the tx; the tag applies to all of it.
		asset, amount := "", 0.0
		if len(r.tx.Legs) > 0 {
			asset, amount = r.tx.Legs[0].AssetKey, r.tx.Legs[0].Amount
		}
		if amount < 0 {
			amount = -amount
		}
		tagged.Items = append(tagged.Items, item(r.id, r.tx.SourceID, asset, amount, r.tx.Time, ""))
	}
	if tagged.Count > 0 {
		p.Groups = append(p.Groups, tagged)
	}
	return p
}
