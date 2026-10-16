package tax

import "time"

// queue holds the open lots of one asset in one wallet, oldest acquisition first.
type queue struct {
	lots []Lot
}

// add inserts l after every lot acquired at or before it, so equal times keep insertion
// order. A lot at or below dust (or with a NaN amount) is ignored: a zero or negative lot
// would corrupt later shortfalls.
func (q *queue) add(l Lot) {
	if !(l.Amount > dust) {
		return
	}
	i := len(q.lots)
	for i > 0 && q.lots[i-1].Acquired.After(l.Acquired) {
		i--
	}
	q.lots = append(q.lots, Lot{})
	copy(q.lots[i+1:], q.lots[i:])
	q.lots[i] = l
}

// take removes amount from the oldest lots first. A lot that is only partly consumed is
// split with its cost pro-rata. short is the part of amount the queue could not cover.
// Residue at or below dust is dropped.
func (q *queue) take(amount float64) (taken []Lot, short float64) {
	need := amount
	for len(q.lots) > 0 && need > dust {
		l := q.lots[0]
		if l.Amount <= need+dust {
			taken = append(taken, l)
			need -= l.Amount
			q.lots = q.lots[1:]
			continue
		}
		part := l
		part.Amount = need
		part.CostUSD = l.CostUSD * need / l.Amount
		taken = append(taken, part)
		q.lots[0].Amount -= need
		q.lots[0].CostUSD -= part.CostUSD
		need = 0
	}
	if need > dust {
		short = need
	}
	return taken, short
}

// total is the amount held across all open lots.
func (q *queue) total() float64 {
	var s float64
	for _, l := range q.lots {
		s += l.Amount
	}
	return s
}

// termOf classifies a disposal by calendar dates in loc (nil means UTC). It is long-term
// only when the sale date is after the acquisition date's one-year anniversary; a sale on
// the anniversary itself is short-term.
func termOf(acquired, sold time.Time, loc *time.Location) Term {
	if loc == nil {
		loc = time.UTC
	}
	date := func(t time.Time) time.Time {
		y, m, d := t.In(loc).Date()
		return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	}
	if date(sold).After(date(acquired).AddDate(1, 0, 0)) {
		return LongTerm
	}
	return ShortTerm
}
