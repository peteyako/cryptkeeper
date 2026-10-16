package solana

type knownMint struct{ Symbol, Name string }

// knownMints maps well-known SPL mints to the ticker exchanges use, so a Solana balance merges
// with the same asset held on Coinbase or Kraken (AssetKey = symbol instead of "spl:<mint>").
//
// Every entry was checked on 2026-10-16: the mint is "isVerified" in the Jupiter token API
// (https://lite-api.jup.ag/tokens/v2/search?query=<mint>), the ticker is a USD market on Kraken
// and/or Coinbase, and Jupiter's USD price agrees with the exchange price to within ~1%.
// The one exception is JUP: Coinbase's JUP product is delisted and its v2 spot price is stale
// (about 1000x too low), whereas Kraken JUP/USD and Jupiter agree. Price JUP from Kraken/Jupiter.
//
// Only add a mint after verifying it the same way: look-alike tokens reuse tickers freely, and
// the mint is the only identity that counts. wSOL is deliberately absent; it merges into SOL.
var knownMints = map[string]knownMint{
	// Stablecoins.
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": {"USDC", "USD Coin"},
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": {"USDT", "Tether USD"},
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": {"PYUSD", "PayPal USD"}, // Token-2022

	// Ecosystem and DeFi tokens.
	"JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN":  {"JUP", "Jupiter"},
	"HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3": {"PYTH", "Pyth Network"},
	"4k3Dyjzvzp8eMZWUXbBCjEvwSkkk59S5iCNLY3QrkX6R": {"RAY", "Raydium"},
	"jtojtomepa8beP8AuQc6eXt5FriJwfFMwQx2v2f9mCL":  {"JTO", "Jito"},
	"85VBFQZC9TZkfaptBWjvUw7YbZjy52A6mjtPGjstQAmQ": {"W", "Wormhole"},
	"rndrizKT3MK1iimdxRdWabcF7Zg7AR5T4nud4EkHBof":  {"RENDER", "Render"},
	"hntyVP6YFm1Hg25TN9WGLqM12b8TQmcknKrdu1oxWux":  {"HNT", "Helium"},
	"orcaEKTdK7LKz57vaAYr9QeNsVEPfiu6QeMU1kektZE":  {"ORCA", "Orca"},
	"KMNo3nJsBXfcpJTVhZcXLW7RmTwTt4GVFE7suUBo9sS":  {"KMNO", "Kamino"},
	"DriFtupJYLTosbwoN8koMbEYSx54aFAVLddWsbksjwg7": {"DRIFT", "Drift"},
	"MNDEFzGvMt87ueuHvVU9VcTqsAP5b3fTGPsHuuPA5ey":  {"MNDE", "Marinade"},
	"TNSRxcUxoT9xBG3de7PiJyTDYu7kskLqcpddxnEJAS6":  {"TNSR", "Tensor"},

	// Memecoins.
	"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263": {"BONK", "Bonk"},
	"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm": {"WIF", "dogwifhat"},
	"6p6xgHyF7AeE6TZkSmFsko444wqoP15icUSqi2jfGiPN": {"TRUMP", "Official Trump"},
	"2zMMhcVQEXDtdE6vsFS7S7D5oUodfJHE8vd1gnBouauv": {"PENGU", "Pudgy Penguins"},
	"7GCihgDB8fe6KNjn2MYtkzZcRjQy3t9GHdC8uHYmW2hr": {"POPCAT", "Popcat"},
}

// KnownMint returns the exchange symbol and display name of a well-known SPL mint.
func KnownMint(mint string) (symbol, name string, ok bool) {
	k, ok := knownMints[mint]
	return k.Symbol, k.Name, ok
}

// MintForSymbol is the reverse of KnownMint: the verified mint for an exchange ticker, so a
// JUP balance on Kraken can be priced by mint like the same token on Solana.
func MintForSymbol(symbol string) (string, bool) {
	for mint, k := range knownMints {
		if k.Symbol == symbol {
			return mint, true
		}
	}
	return "", false
}
