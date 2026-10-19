package coinbase

// Activity: the account's transaction history from the Coinbase App v2 API.
//
//	GET /v2/accounts                              every wallet/vault/fiat account (cached in State)
//	GET /v2/accounts/{id}/transactions            one account's transactions, newest first
//	GET /v2/accounts/{id}/transactions/{tx_id}    re-check of a transaction that was still pending
//
// Pagination follows the v2 cursor scheme (https://docs.cdp.coinbase.com/coinbase-app/api-architecture/pagination):
// a page is `limit` (max 100) transactions in `order` (default desc); `pagination.next_uri` is the
// ready-made request for the next, older page and is empty on the last page. `starting_after=<id>`
// pages toward older entries. `ending_before=<id>` pages toward newer ones; see newActivity for the
// assumption this code makes about it.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"cryptkeeper/internal/core"
	"cryptkeeper/internal/httpx"
)

var _ core.ActivitySource = (*Source)(nil)

const (
	defaultActivityBudget = 20
	txPageSize            = 100 // documented maximum for v2 "limit"

	// A pending transaction is re-checked on later calls until it settles, at most pendingMaxTracked
	// per account.
	pendingMaxTracked = 25
)

var (
	// accountsTTL is how long the account list kept in State is trusted. Listing accounts costs a
	// request on every call otherwise, which a small budget cannot afford. (A variable so tests
	// can expire the list without moving the clock that signs JWTs.)
	accountsTTL = 30 * time.Minute

	// pendingMaxAge is how long after its creation a pending transaction is still re-checked.
	pendingMaxAge = 14 * 24 * time.Hour
)

// ---------------------------------------------------------------------------------------------
// State
// ---------------------------------------------------------------------------------------------

// activityState is the opaque State handed back to the caller.
type activityState struct {
	// Since is the history horizon (unix seconds, 0 = unbounded) the backfill has been run for.
	Since int64 `json:"since,omitempty"`
	// Seq counts account visits; an account's Synced is the Seq value of its last visit, so every
	// call serves the least recently visited accounts first and a small budget still rotates
	// through all of them.
	Seq int64 `json:"seq,omitempty"`
	// Calls counts Activity calls; with a budget of one request it alternates new-activity and
	// backfill calls so that neither starves.
	Calls    int64                 `json:"calls,omitempty"`
	Wallets  []walletMeta          `json:"wallets,omitempty"`
	ListedAt int64                 `json:"listedAt,omitempty"` // unix seconds the Wallets list was fetched
	Accounts map[string]*acctState `json:"accounts,omitempty"`
}

// walletMeta is the part of a v2 account that mapping needs.
type walletMeta struct {
	ID   string `json:"id"`
	Code string `json:"code"`           // currency code as the API reports it
	Name string `json:"name,omitempty"` // currency name, used to spot staked wallets
}

// acctState is the cursor state of one account.
type acctState struct {
	// Newest is the id of the newest transaction seen: new activity is everything newer than it.
	Newest string `json:"newest,omitempty"`
	// NextURI is the next backfill request (path and query), starting at the page after the last
	// one processed. When the backfill stopped at the Since horizon it is the page that crossed
	// it, so a later, earlier horizon resumes there.
	NextURI string `json:"nextURI,omitempty"`
	// Done means the backfill needs no further requests for the current Since.
	Done    bool         `json:"done,omitempty"`
	Pending []pendingRef `json:"pending,omitempty"`
	Synced  int64        `json:"synced,omitempty"`
}

type pendingRef struct {
	ID string `json:"id"`
	T  int64  `json:"t"` // created_at, unix seconds
}

func loadState(raw []byte) *activityState {
	st := &activityState{}
	// State is only a set of cursors: if it cannot be read, starting over re-fetches history and
	// the caller's store (keyed by Tx.ID) absorbs the repeats.
	if len(raw) == 0 || json.Unmarshal(raw, st) != nil {
		st = &activityState{}
	}
	if st.Accounts == nil {
		st.Accounts = map[string]*acctState{}
	}
	return st
}

func (st *activityState) acct(id string) *acctState {
	a := st.Accounts[id]
	if a == nil {
		a = &acctState{}
		st.Accounts[id] = a
	}
	return a
}

// ---------------------------------------------------------------------------------------------
// Wire types
// ---------------------------------------------------------------------------------------------

type v2Money struct {
	Amount   decStr `json:"amount"`
	Currency string `json:"currency"`
}

// v2Party is a "to" or "from" object. Its shape depends on resource: a blockchain address, an
// email, a user, another of the user's accounts or a network ("bitcoin_network").
type v2Party struct {
	ID       string `json:"id"`
	Resource string `json:"resource"`
	Address  string `json:"address"`
	Email    string `json:"email"`
	Name     string `json:"name"`
}

// label is the best human-readable identity of the party, "" when there is none worth showing.
func (p *v2Party) label() string {
	switch {
	case p == nil:
		return ""
	case p.Address != "":
		return p.Address
	case p.Email != "":
		return p.Email
	case p.Name != "":
		return p.Name
	case p.Resource != "" && p.Resource != "user" && p.Resource != "account":
		return p.Resource
	}
	return ""
}

type v2Network struct {
	Status            string   `json:"status"`
	Name              string   `json:"name"`
	Hash              string   `json:"hash"`
	TransactionURL    string   `json:"transaction_url"`
	TransactionFee    *v2Money `json:"transaction_fee"`
	TransactionAmount *v2Money `json:"transaction_amount"`
	Confirmations     any      `json:"confirmations"`
}

type v2Tx struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	Status       string     `json:"status"`
	Amount       v2Money    `json:"amount"`
	NativeAmount v2Money    `json:"native_amount"`
	Description  string     `json:"description"`
	CreatedAt    string     `json:"created_at"`
	Network      *v2Network `json:"network"`
	To           *v2Party   `json:"to"`
	From         *v2Party   `json:"from"`
	Details      *struct {
		Title    string `json:"title"`
		Subtitle string `json:"subtitle"`
	} `json:"details"`
}

type v2TxList struct {
	Pagination struct {
		NextURI *string `json:"next_uri"`
	} `json:"pagination"`
	Data []v2Tx `json:"data"`
}

type v2TxOne struct {
	Data v2Tx `json:"data"`
}

// ---------------------------------------------------------------------------------------------
// Mapping
// ---------------------------------------------------------------------------------------------

func (d decimal) sign() int {
	if d.r == nil {
		return 0
	}
	return d.r.Sign()
}

// abs and neg return a copy.
func (d decimal) abs() decimal {
	if d.r == nil {
		return decimal{r: new(big.Rat)}
	}
	return decimal{r: new(big.Rat).Abs(d.r), scale: d.scale}
}

func (d decimal) neg() decimal {
	if d.r == nil {
		return decimal{r: new(big.Rat)}
	}
	return decimal{r: new(big.Rat).Neg(d.r), scale: d.scale}
}

func kindOf(typ string, amount decimal) core.TxKind {
	switch typ {
	case "buy", "sell", "trade", "advanced_trade_fill", "retail_simple_dust":
		return core.TxTrade
	case "send":
		if amount.sign() > 0 {
			return core.TxReceive // an incoming "send" is the other side of someone's send
		}
		return core.TxSend
	case "receive":
		return core.TxReceive
	case "staking_reward", "inflation_reward", "interest", "earn_payout", "reward",
		"incentives_rewards_payout":
		return core.TxReward
	case "fiat_deposit", "fiat_withdrawal", "exchange_deposit", "exchange_withdrawal",
		"pro_deposit", "pro_withdrawal", "transfer",
		"staking_transfer", "unstaking_transfer", "intx_deposit", "intx_withdrawal", "vault_withdrawal":
		return core.TxTransfer
	}
	return core.TxOther
}

func statusOf(s string) core.TxStatus {
	switch s {
	case "completed":
		return core.TxConfirmed
	case "failed", "canceled", "cancelled", "expired":
		return core.TxFailed
	}
	// "pending", the vault "waiting_for_*" states and anything new: not settled as far as we know.
	return core.TxPending
}

// mapTx converts one v2 transaction of account w into a core.Tx with a single leg in the
// account's currency.
func (s *Source) mapTx(w walletMeta, t v2Tx) (core.Tx, error) {
	created, err := time.Parse(time.RFC3339, t.CreatedAt)
	if err != nil {
		return core.Tx{}, fmt.Errorf("transaction %s: created_at %q: %w", t.ID, t.CreatedAt, err)
	}
	amt, err := parseDecimal(string(t.Amount.Amount))
	if err != nil {
		return core.Tx{}, fmt.Errorf("transaction %s amount: %w", t.ID, err)
	}

	// Staked wallets are recognised by the account's currency name, which only describes the
	// account's own currency.
	code, name := t.Amount.Currency, ""
	if code == "" || strings.EqualFold(code, w.Code) {
		code, name = w.Code, w.Name
	}
	sym, _ := normalizeCurrency(code, name)

	kind := kindOf(t.Type, amt)
	leg := core.Leg{AssetKey: sym, Symbol: sym, Amount: amt.float()}
	switch {
	case strings.EqualFold(t.NativeAmount.Currency, "USD") && strings.TrimSpace(string(t.NativeAmount.Amount)) != "":
		if usd, err := parseDecimal(string(t.NativeAmount.Amount)); err == nil {
			v := usd.float()
			leg.USD = &v
		}
	case sym == "USD":
		v := leg.Amount
		leg.USD = &v
	}

	tx := core.Tx{
		ID:         core.TxID(s.id, t.ID),
		SourceID:   s.id,
		ExternalID: t.ID,
		Time:       created.UTC(),
		Kind:       kind,
		Status:     statusOf(t.Status),
		Legs:       []core.Leg{leg},
	}

	if n := t.Network; n != nil {
		tx.TxHash = n.Hash
		tx.ExplorerURL = n.TransactionURL
		switch c := n.Confirmations.(type) {
		case float64:
			tx.Confirmations = max(0, int(c))
		case string:
			if v, err := strconv.Atoi(c); err == nil {
				tx.Confirmations = max(0, v)
			}
		}
		if kind == core.TxSend {
			if err := applyFee(&tx, n, amt, sym); err != nil {
				return core.Tx{}, fmt.Errorf("transaction %s: %w", t.ID, err)
			}
		}
	}

	switch kind {
	case core.TxSend:
		tx.Counterparty = t.To.label()
	case core.TxReceive:
		tx.Counterparty = t.From.label()
	case core.TxTransfer:
		if amt.sign() < 0 {
			tx.Counterparty = t.To.label()
		} else {
			tx.Counterparty = t.From.label()
		}
	}

	tx.Note = strings.TrimSpace(t.Description)
	if tx.Note == "" && t.Details != nil {
		tx.Note = joinNonEmpty(" · ", t.Details.Title, t.Details.Subtitle)
	}
	if kind == core.TxOther {
		tx.Note = joinNonEmpty(" · ", "coinbase type: "+t.Type, tx.Note)
	}
	return tx, nil
}

func joinNonEmpty(sep string, parts ...string) string {
	var out []string
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return strings.Join(out, sep)
}

// applyFee sets tx.Fee from the network fee of a send. Per core.Tx the fee is not part of the leg,
// so when network.transaction_amount proves that the debited amount already includes the fee
// (amount == transaction_amount + fee) the leg is cut back to the amount that was sent. The docs do
// not say whether amount includes the fee, so without that proof the leg stays as Coinbase gives it.
func applyFee(tx *core.Tx, n *v2Network, amt decimal, legSym string) error {
	if n.TransactionFee == nil || n.TransactionFee.Amount == "" {
		return nil
	}
	fee, err := parseDecimal(string(n.TransactionFee.Amount))
	if err != nil {
		return fmt.Errorf("network fee: %w", err)
	}
	if fee.isZero() {
		return nil
	}
	feeSym := legSym
	if c := n.TransactionFee.Currency; c != "" {
		feeSym, _ = normalizeCurrency(c, "")
	}
	tx.Fee = &core.Leg{AssetKey: feeSym, Symbol: feeSym, Amount: fee.neg().float()}
	if feeSym == "USD" {
		v := tx.Fee.Amount
		tx.Fee.USD = &v
	}

	if ta := n.TransactionAmount; ta != nil && feeSym == legSym && amt.sign() < 0 {
		if sent, err := parseDecimal(string(ta.Amount)); err == nil && !sent.isZero() &&
			sent.abs().plus(fee.abs()).equal(amt.abs()) {
			tx.Legs[0].Amount = sent.abs().neg().float()
			if tx.Legs[0].USD != nil {
				// native_amount covered the whole debit; scale it to the amount actually sent.
				v := *tx.Legs[0].USD * tx.Legs[0].Amount / amt.float()
				tx.Legs[0].USD = &v
			}
		}
	}
	return nil
}

// ---------------------------------------------------------------------------------------------
// Activity
// ---------------------------------------------------------------------------------------------

// dataErr marks an answer that arrived fine but cannot be used (bad amount, bad date, bad
// next_uri). It sets one account aside instead of ending the call.
type dataErr struct{ err error }

func (e dataErr) Error() string { return e.err.Error() }
func (e dataErr) Unwrap() error { return e.err }

// activityRun is one Activity call.
type activityRun struct {
	s      *Source
	ctx    context.Context
	st     *activityState
	hdr    map[string]string
	now    time.Time
	since  time.Time
	budget int
	used   int
	// limitNew caps the requests that new-activity checks and pending re-checks may use, keeping
	// the rest of the budget for the backfill so that it cannot starve.
	limitNew int

	txs   []core.Tx
	seen  map[string]bool
	errs  []error
	fatal error
	stop  bool
	ok    int             // transaction requests that succeeded
	bad   map[string]bool // accounts set aside in this call after an error of their own
	// truncated is set when the budget ran out before the account list was complete.
	truncated bool
}

// take spends one request of the budget up to limit.
func (r *activityRun) take(limit int) bool {
	if r.used >= limit {
		return false
	}
	r.used++
	return true
}

func txListPath(accountID string, q url.Values) string {
	return v2AccountsPath + "/" + url.PathEscape(accountID) + "/transactions?" + q.Encode()
}

func firstPageQuery() url.Values {
	return url.Values{"limit": {strconv.Itoa(txPageSize)}, "order": {"desc"}}
}

// fail records err for account id. Errors that say something about the whole service (transport,
// 429, 5xx, cancellation) end the call; the rest only set that account aside.
func (r *activityRun) fail(id string, err error) {
	var se *httpx.StatusError
	var de dataErr
	var syn *json.SyntaxError
	var typ *json.UnmarshalTypeError
	switch {
	case errors.As(err, &se):
		if se.Status == http.StatusTooManyRequests || se.Status >= 500 {
			r.fatal, r.stop = err, true
			return
		}
	case errors.As(err, &de), errors.As(err, &syn), errors.As(err, &typ):
	default:
		r.fatal, r.stop = err, true
		return
	}
	r.bad[id] = true
	r.errs = append(r.errs, fmt.Errorf("coinbase: account %s: %w", id, err))
}

// listAccounts returns the wallets, from State when the cached list is fresh. A fresh listing is
// paged with next_uri and charged against the budget.
func (r *activityRun) listAccounts() ([]walletMeta, error) {
	if age := r.now.Sub(time.Unix(r.st.ListedAt, 0)); len(r.st.Wallets) > 0 && age >= 0 && age < accountsTTL {
		return r.st.Wallets, nil
	}
	var out []walletMeta
	next := fmt.Sprintf("%s?limit=%d", v2AccountsPath, v2PageSize)
	seen := map[string]bool{}
	for page := 0; next != ""; page++ {
		if page >= maxPages {
			return nil, fmt.Errorf("coinbase: accounts: more than %d pages", maxPages)
		}
		if seen[next] {
			return nil, errors.New("coinbase: accounts: pagination loop: next_uri repeated")
		}
		seen[next] = true
		if !r.take(r.budget) {
			// Out of budget mid-listing: work with what we have, but do not cache a partial list.
			r.stop, r.truncated = true, true
			return out, nil
		}
		var resp v2Response
		if err := r.s.getJSON(r.ctx, next, r.hdr, &resp); err != nil {
			return nil, fmt.Errorf("coinbase: list accounts: %w", err)
		}
		for _, a := range resp.Data {
			code := a.Currency.Code
			if code == "" {
				code = a.Balance.Currency
			}
			if a.ID == "" || strings.TrimSpace(code) == "" {
				continue
			}
			out = append(out, walletMeta{ID: a.ID, Code: code, Name: a.Currency.Name})
		}
		var err error
		if next, err = cleanNextURI(resp.Pagination.NextURI); err != nil {
			return nil, fmt.Errorf("coinbase: list accounts: %w", err)
		}
	}
	r.st.Wallets, r.st.ListedAt = out, r.now.Unix()
	return out, nil
}

// consume maps a page of transactions of account w and adds the ones inside the Since horizon to
// the result. It returns the id of the newest transaction on the page (by created_at, so the page
// order does not matter) and whether the page reached back past Since. Nothing is added when any
// transaction on the page cannot be mapped.
func (r *activityRun) consume(w walletMeta, st *acctState, data []v2Tx) (newest string, cutoff bool, err error) {
	var batch []core.Tx
	var newestAt time.Time
	for _, t := range data {
		tx, err := r.s.mapTx(w, t)
		if err != nil {
			return "", false, dataErr{err}
		}
		if newest == "" || tx.Time.After(newestAt) {
			newest, newestAt = t.ID, tx.Time
		}
		if !r.since.IsZero() && tx.Time.Before(r.since) {
			cutoff = true
			continue
		}
		batch = append(batch, tx)
	}
	for _, tx := range batch {
		if r.seen[tx.ID] {
			continue
		}
		r.seen[tx.ID] = true
		r.txs = append(r.txs, tx)
		st.track(tx)
	}
	return newest, cutoff, nil
}

// track keeps st.Pending in step with a transaction that was just emitted.
func (st *acctState) track(tx core.Tx) {
	i := -1
	for j, p := range st.Pending {
		if p.ID == tx.ExternalID {
			i = j
		}
	}
	switch {
	case tx.Status != core.TxPending && i >= 0:
		st.Pending = append(st.Pending[:i], st.Pending[i+1:]...)
	case tx.Status == core.TxPending && i < 0:
		st.Pending = append(st.Pending, pendingRef{ID: tx.ExternalID, T: tx.Time.Unix()})
		if over := len(st.Pending) - pendingMaxTracked; over > 0 {
			st.Pending = st.Pending[over:]
		}
	}
}

// newActivity fetches what happened in account w since the last call.
//
// With no Newest yet it fetches the first (newest) page, which also starts the backfill. After
// that it asks for `ending_before=<Newest>` with order=desc. The v2 docs name the cursors but do
// not spell out their direction; this code relies on the Stripe-style meaning they were modelled
// on: ending_before=X returns the page that comes *before* X in the list, i.e. the `limit`
// transactions immediately newer than X. Under that meaning every page is contiguous with the
// previous one, so Newest can advance page by page and stopping on a short page or when the
// budget runs out never leaves a gap. (If the API instead returned the newest `limit` entries,
// more than `limit` new transactions between two calls would leave a gap.)
func (r *activityRun) newActivity(w walletMeta, st *acctState) {
	for {
		if !r.take(r.limitNew) {
			r.stop = true
			return
		}
		initial := st.Newest == ""
		q := firstPageQuery()
		if !initial {
			q.Set("ending_before", st.Newest)
		}
		uri := txListPath(w.ID, q)
		var resp v2TxList
		if err := r.s.getJSON(r.ctx, uri, r.hdr, &resp); err != nil {
			r.fail(w.ID, err)
			return
		}
		r.ok++
		next, err := cleanNextURI(resp.Pagination.NextURI)
		if err != nil {
			r.fail(w.ID, dataErr{err})
			return
		}
		data := resp.Data
		if !initial { // an inclusive cursor must not hand back the transaction it was given
			data = nil
			for _, t := range resp.Data {
				if t.ID != st.Newest {
					data = append(data, t)
				}
			}
		}
		newest, cutoff, err := r.consume(w, st, data)
		if err != nil {
			r.fail(w.ID, err)
			return
		}
		if newest != "" {
			st.Newest = newest
		}

		if initial {
			// The first page doubles as the first backfill page.
			switch {
			case cutoff:
				st.Done, st.NextURI = true, uri
			case next == "":
				st.Done, st.NextURI = true, ""
			default:
				st.Done, st.NextURI = false, next
			}
			break
		}
		if len(resp.Data) < txPageSize || newest == "" {
			break
		}
	}
	r.st.Seq++
	st.Synced = r.st.Seq
}

// backfillOnce fetches one page of older history for account w.
func (r *activityRun) backfillOnce(w walletMeta, st *acctState) {
	uri := st.NextURI
	if uri == "" {
		uri = txListPath(w.ID, firstPageQuery())
	}
	var resp v2TxList
	if err := r.s.getJSON(r.ctx, uri, r.hdr, &resp); err != nil {
		r.fail(w.ID, err)
		return
	}
	r.ok++
	next, err := cleanNextURI(resp.Pagination.NextURI)
	if err != nil {
		r.fail(w.ID, dataErr{err})
		return
	}
	newest, cutoff, err := r.consume(w, st, resp.Data)
	if err != nil {
		r.fail(w.ID, err)
		return
	}
	if st.Newest == "" {
		st.Newest = newest
	}
	switch {
	case cutoff:
		st.Done, st.NextURI = true, uri
	case next == "":
		st.Done, st.NextURI = true, ""
	default:
		st.NextURI = next
	}
}

// refreshPending re-reads the transactions of account w that were pending when last seen and emits
// the ones that have settled since.
func (r *activityRun) refreshPending(w walletMeta, st *acctState) {
	var keep []pendingRef
	for i, p := range st.Pending {
		if r.now.Sub(time.Unix(p.T, 0)) > pendingMaxAge {
			continue // given up; it stays pending in the caller's store
		}
		if r.seen[core.TxID(r.s.id, p.ID)] {
			keep = append(keep, p) // fetched in this very call
			continue
		}
		if r.stop || !r.take(r.limitNew) {
			r.stop = true
			keep = append(keep, st.Pending[i:]...)
			break
		}
		var resp v2TxOne
		path := v2AccountsPath + "/" + url.PathEscape(w.ID) + "/transactions/" + url.PathEscape(p.ID)
		if err := r.s.getJSON(r.ctx, path, r.hdr, &resp); err != nil {
			var se *httpx.StatusError
			if errors.As(err, &se) && se.Status == http.StatusNotFound {
				continue // gone
			}
			keep = append(keep, st.Pending[i:]...)
			r.fail(w.ID, err)
			break
		}
		r.ok++
		tx, err := r.s.mapTx(w, resp.Data)
		if err != nil {
			keep = append(keep, p)
			r.fail(w.ID, dataErr{err})
			continue
		}
		if tx.Status == core.TxPending {
			keep = append(keep, p)
			continue
		}
		if !r.seen[tx.ID] {
			r.seen[tx.ID] = true
			r.txs = append(r.txs, tx)
		}
	}
	st.Pending = keep
}

// Activity returns Coinbase transactions as core.Tx.
//
// Each call spends at most req.Budget requests (default 20): listing accounts when the cached list
// is stale, then per account the new activity since the last call, then re-checks of transactions
// that were pending, then the backfill of older history down to req.Since, one page per account in
// turn. A quarter of the budget is kept back for the backfill. State holds the cursors, so the
// next call carries on from where this one stopped without repeating transactions (except for
// those that settled, which are returned again with their new status).
//
// A request that fails ends the call (a 4xx or malformed answer for one account only sets that
// account aside for this call). If other requests succeeded, the call ends cleanly: the
// transactions fetched so far and a State that matches them are returned without an error, since
// callers that drop a result along with its error would lose that progress, and a problem that
// persists is reported once no request succeeds any more. When nothing succeeded, the error is
// returned, together with the (empty) result.
func (s *Source) Activity(ctx context.Context, req core.ActivityRequest) (core.ActivityResult, error) {
	budget := req.Budget
	if budget <= 0 {
		budget = defaultActivityBudget
	}
	r := &activityRun{
		s:      s,
		ctx:    ctx,
		st:     loadState(req.State),
		hdr:    map[string]string{"CB-VERSION": v2Version},
		now:    s.now(),
		since:  req.Since,
		budget: budget,
		seen:   map[string]bool{},
		bad:    map[string]bool{},
	}
	st := r.st

	// An earlier horizon than the one the backfill ran for reopens the finished accounts.
	if sinceU := sinceUnix(req.Since); st.Since != 0 && (sinceU == 0 || sinceU < st.Since) {
		for _, a := range st.Accounts {
			if a.NextURI != "" {
				a.Done = false
			}
		}
	}
	st.Since = sinceUnix(req.Since)

	wallets, err := r.listAccounts()
	if err != nil {
		return core.ActivityResult{State: req.State}, err
	}

	// Least recently visited first; ties keep the listing order.
	order := append([]walletMeta(nil), wallets...)
	sort.SliceStable(order, func(i, j int) bool {
		return st.acct(order[i].ID).Synced < st.acct(order[j].ID).Synced
	})

	r.limitNew = budget
	for _, w := range order {
		if !st.acct(w.ID).Done {
			reserve := budget / 4
			if budget >= 2 {
				reserve = max(1, reserve)
			} else if st.Calls%2 == 1 {
				reserve = 1
			}
			r.limitNew = budget - reserve
			break
		}
	}
	st.Calls++

	for _, w := range order {
		if r.stop {
			break
		}
		r.newActivity(w, st.acct(w.ID))
	}
	for _, w := range order {
		if r.stop {
			break
		}
		if a := st.acct(w.ID); len(a.Pending) > 0 && !r.bad[w.ID] {
			r.refreshPending(w, a)
		}
	}
	if r.fatal == nil {
		r.stop = false // the new-activity share is spent, not the budget
	}
	for progressed := true; progressed && !r.stop; {
		progressed = false
		for _, w := range order {
			a := st.acct(w.ID)
			if a.Done || r.bad[w.ID] {
				continue
			}
			if !r.take(budget) {
				r.stop = true
				break
			}
			progressed = true
			r.backfillOnce(w, a)
			if r.fatal != nil {
				break
			}
		}
	}

	// An account that fails on its own (403, bad data) cannot be backfilled and does not hold
	// BackfillDone back; it is tried again on every call.
	done := !r.truncated
	for _, w := range wallets {
		if !st.acct(w.ID).Done && !r.bad[w.ID] {
			done = false
		}
	}
	state, merr := json.Marshal(st)
	if merr != nil {
		return core.ActivityResult{}, merr
	}
	sort.SliceStable(r.txs, func(i, j int) bool { return r.txs[i].Time.After(r.txs[j].Time) })
	res := core.ActivityResult{Txs: r.txs, State: state, BackfillDone: done}
	if r.ok > 0 && ctx.Err() == nil {
		// Some requests worked: keep that progress and end the call cleanly rather than failing it.
		// A problem that persists shows up as an error once no request succeeds any more.
		return res, nil
	}
	res.BackfillDone = false
	if r.fatal != nil {
		return res, fmt.Errorf("coinbase: activity: %w", r.fatal)
	}
	return res, errors.Join(r.errs...)
}

func sinceUnix(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}
