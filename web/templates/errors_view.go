package templates

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrorView is a source error in plain language.
type ErrorView struct {
	Text string // what went wrong, and when CryptKeeper tries again
	Fix  string // what the user can do about it; "" when waiting is all there is
	Raw  string // the original error for the Details toggle; "" when Text already says it all
}

// errKind is the family of a source error, read from its text.
type errKind int

const (
	errOther errKind = iota
	errTimeout
	errRateLimit
	errAuth
	errNetwork
	errServer
	errInternal
	errNeedsEdit // the source itself must be edited (a missing API key); the text says how
)

var httpStatusRe = regexp.MustCompile(`\bHTTP (\d{3})\b`)

// classifyErr sorts a raw error by what the user would do about it. Errors arrive as text
// (they are stored), so this matches the wording of the Go and source errors CryptKeeper makes.
func classifyErr(raw string) (errKind, int) {
	l := strings.ToLower(raw)
	status := 0
	if m := httpStatusRe.FindStringSubmatch(raw); m != nil {
		status, _ = strconv.Atoi(m[1])
	}
	hasAny := func(subs ...string) bool {
		for _, s := range subs {
			if strings.Contains(l, s) {
				return true
			}
		}
		return false
	}
	switch {
	case strings.Contains(l, "edit this source"):
		return errNeedsEdit, status
	case status == 429 || hasAny("rate limit", "too many requests", "throttled", "temporary lockout"):
		return errRateLimit, status
	case status == 401 || status == 403 || hasAny("invalid key", "invalid signature", "unauthorized", "forbidden", "permission denied"):
		return errAuth, status
	case hasAny("deadline exceeded", "timeout", "timed out"):
		return errTimeout, status
	case hasAny("no such host", "connection refused", "network is unreachable", "connection reset", "tls:", "certificate"):
		return errNetwork, status
	case status >= 500:
		return errServer, status
	case strings.HasPrefix(l, "panic"):
		return errInternal, status
	}
	return errOther, status
}

// serviceName is what a source talks to, as a sentence would name it.
func serviceName(sourceType string) string {
	switch sourceType {
	case "solana":
		return "the Solana RPC"
	case "bitcoin":
		return "the Esplora server"
	case "coinbase":
		return "Coinbase"
	case "kraken":
		return "Kraken"
	}
	return "the source"
}

// retryText says when CryptKeeper tries again: "CryptKeeper tries again every 15 min."
func retryText(every time.Duration) string {
	if every <= 0 {
		return "CryptKeeper tries again on the next sync."
	}
	return "CryptKeeper tries again " + Every(every) + "."
}

// Every words an interval: "every minute", "every 15 min", "every hour", "every 2 hours".
func Every(d time.Duration) string {
	switch {
	case d == time.Minute:
		return "every minute"
	case d < time.Hour || d%time.Hour != 0:
		return "every " + strconv.Itoa(int(d.Round(time.Minute)/time.Minute)) + " min"
	case d == time.Hour:
		return "every hour"
	}
	return "every " + strconv.Itoa(int(d/time.Hour)) + " hours"
}

// slowFix is the advice for a slow or rate-limited source: a public endpoint is shared, and
// a private one is the cure.
func slowFix(sourceType string, public bool) string {
	if !public {
		return ""
	}
	switch sourceType {
	case "solana":
		return "The public Solana RPC is slow and rate-limited. A free Helius RPC URL in config.yaml (solana.rpcUrl) fixes this."
	case "bitcoin":
		return "Public Esplora servers are shared and rate-limited. Pointing bitcoin.esploraUrl in config.yaml at your own node fixes this."
	}
	return ""
}

// ExplainError turns a source's raw error into plain language. sourceType is the source's
// type (solana, bitcoin, coinbase, kraken), public is set when it reads from a shared public
// endpoint (the public Solana RPC, mempool.space), and every is how often CryptKeeper retries
// (0 when unknown). An empty raw error gives a zero ErrorView.
func ExplainError(raw, sourceType string, public bool, every time.Duration) ErrorView {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ErrorView{}
	}
	svc := serviceName(sourceType)
	retry := retryText(every)
	kind, status := classifyErr(raw)
	v := ErrorView{Raw: raw}
	switch kind {
	case errNeedsEdit:
		// Already written for people ("needs its API key and secret: edit this source to
		// add them"); only the capitalization changes.
		what, fix, _ := strings.Cut(raw, ":")
		return ErrorView{Text: upperFirst(strings.TrimSpace(what)) + ".", Fix: upperFirst(strings.TrimSpace(fix)) + "."}
	case errTimeout:
		v.Text = "Timed out: " + svc + " was too slow to answer. Anything fetched before that is kept. " + retry
		v.Fix = slowFix(sourceType, public)
	case errRateLimit:
		v.Text = "Rate limited: " + svc + " asked CryptKeeper to slow down. " + retry
		v.Fix = slowFix(sourceType, public)
	case errAuth:
		v.Text = upperFirst(svc) + " rejected the API key."
		switch sourceType {
		case "coinbase":
			v.Fix = "Check that the key still exists in the Coinbase Developer Platform and has View permission, then edit this source."
		case "kraken":
			v.Fix = "Check that the key still exists and has Query Funds and Query Ledger Entries permission, then edit this source."
		default:
			v.Fix = "Check the key, then edit this source."
		}
	case errNetwork:
		v.Text = "Couldn't reach " + svc + ". " + retry
		v.Fix = "Check this machine's internet connection, and the URL in config.yaml if you changed it."
	case errServer:
		v.Text = upperFirst(svc) + " had a problem on its side (HTTP " + strconv.Itoa(status) + "). " + retry
	case errInternal:
		v.Text = "CryptKeeper hit an internal error with this source. " + retry
	default:
		v.Text = "The last attempt failed. " + retry
	}
	return v
}

func upperFirst(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
