// Command crypto-portfolio is CryptKeep: a self-hosted, read-only crypto portfolio monitor.
//
//	crypto-portfolio serve [-config config.yaml] [-data ./data] [-env .env] [-addr :8787]
//	crypto-portfolio portfolios export [-o file.yaml] [-id ID]
//	crypto-portfolio portfolios import file.yaml [-replace]
//	crypto-portfolio backup export [-o file.yaml] [-secrets]
//	crypto-portfolio backup import file.yaml [-replace]
//	crypto-portfolio sources export [-o file.yaml] [-secrets]
//	crypto-portfolio sources import file.yaml [-replace]
//	crypto-portfolio healthcheck [-addr 127.0.0.1:8787]
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata" // TZ works in the distroless image (no system zoneinfo)

	"crypto-portfolio/internal/activity"
	"crypto-portfolio/internal/alerts"
	"crypto-portfolio/internal/backup"
	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/costbasis"
	"crypto-portfolio/internal/httpx"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/prices"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/server"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/bitcoin"
	"crypto-portfolio/internal/sources/brokerage"
	"crypto-portfolio/internal/sources/coinbase"
	"crypto-portfolio/internal/sources/kraken"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/sources/solana"
	"crypto-portfolio/internal/store"
	"crypto-portfolio/internal/tax"
	"crypto-portfolio/internal/yamlfile"
)

var version = "dev"

func main() {
	args := os.Args[1:]
	cmd := "serve"
	if len(args) > 0 && args[0] != "" && args[0][0] != '-' {
		cmd, args = args[0], args[1:]
	}
	var err error
	switch cmd {
	case "serve":
		err = serve(args)
	case "sources":
		err = sourcesCmd(args)
	case "portfolios":
		err = portfoliosCmd(args)
	case "backup":
		err = backupCmd(args)
	case "healthcheck":
		err = healthcheck(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (serve, backup, portfolios, sources, healthcheck, version)", cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type app struct {
	cfg        config.Config
	dataDir    string
	log        *slog.Logger
	db         *store.DB
	box        *secrets.Box // the secret key: seals exchange secrets and encrypted exports
	client     *httpx.Client
	registry   *sources.Registry
	portfolios *portfolios.Store
	prices     *prices.Service
	// blockstream is the Blockstream API key shared by every Bitcoin wallet.
	blockstream *bitcoin.BlockstreamAuth
	activity    *activity.Store
	costbasis   *costbasis.ManualStore
	alertRules  *alerts.RuleStore
	taxSet      *tax.SettingsStore // data/tax.yaml
	taxYears    *tax.YearStore     // data/tax/<year>.yaml
}

// dbName is the SQLite database in the data directory. Before the app was renamed from
// Vault to CryptKeep it was legacyDBName.
const (
	dbName       = "cryptkeep.db"
	legacyDBName = "vault.db"
)

// renameLegacyDB moves a database still under the Vault-era name, with its WAL and shared
// memory files, to dbName so an existing install keeps its history. The main file moves
// last: if a rename fails partway, the next start finds it under the old name and retries.
// Once dbName exists nothing is touched.
func renameLegacyDB(dataDir string) error {
	dst := filepath.Join(dataDir, dbName)
	if _, err := os.Stat(dst); !errors.Is(err, os.ErrNotExist) {
		return err
	}
	src := filepath.Join(dataDir, legacyDBName)
	for _, suffix := range []string{"-wal", "-shm", ""} {
		if err := os.Rename(src+suffix, dst+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("moving %s to %s: %w", legacyDBName+suffix, dbName+suffix, err)
		}
	}
	return nil
}

// setup loads config, opens the database and the sources registry.
func setup(fs *flag.FlagSet, args []string) (*app, *flag.FlagSet, error) {
	cfgPath := fs.String("config", "config.yaml", "server settings file")
	dataDir := fs.String("data", "data", "data directory (sources, portfolios, database, backups)")
	envPath := fs.String("env", ".env", "optional KEY=VALUE file for secrets")
	if err := parseInterspersed(fs, args); err != nil {
		return nil, fs, err
	}
	if err := config.LoadDotEnv(*envPath); err != nil {
		return nil, fs, err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return nil, fs, err
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return nil, fs, err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	key, err := secrets.LoadKey(cfg.SecretKey, filepath.Join(*dataDir, "secret.key"))
	if err != nil {
		return nil, fs, err
	}
	box, err := secrets.NewBox(key)
	if err != nil {
		return nil, fs, err
	}
	if err := renameLegacyDB(*dataDir); err != nil {
		return nil, fs, err
	}
	db, err := store.Open(filepath.Join(*dataDir, dbName))
	if err != nil {
		return nil, fs, err
	}
	a := &app{cfg: cfg, dataDir: *dataDir, log: log, db: db, box: box, client: httpx.New()}
	a.blockstream = blockstreamAuth(cfg, db, box, a.client, log)
	a.prices = prices.New(a.client, db, prices.Options{TTL: time.Minute, StaleMax: cfg.StaleMax(), CoinGeckoKey: coinGeckoKey(cfg.CoinGeckoKey, db, box)})
	meta := solana.NewSQLiteMetaCache(db, 24*time.Hour)
	reg, err := sources.NewRegistry(filepath.Join(*dataDir, "sources.yaml"), filepath.Join(*dataDir, "backups"), box, a.factory(meta))
	if err != nil {
		db.Close()
		return nil, fs, err
	}
	a.registry = reg
	ps, err := portfolios.NewStore(filepath.Join(*dataDir, "portfolios.yaml"), filepath.Join(*dataDir, "backups"))
	if err != nil {
		db.Close()
		return nil, fs, err
	}
	a.portfolios = ps
	a.activity = &activity.Store{DB: db}
	if a.costbasis, err = costbasis.NewManualStore(filepath.Join(*dataDir, "costbasis.yaml"), filepath.Join(*dataDir, "backups")); err != nil {
		db.Close()
		return nil, fs, err
	}
	if a.alertRules, err = alerts.NewRuleStore(filepath.Join(*dataDir, "alerts.yaml"), filepath.Join(*dataDir, "backups")); err != nil {
		db.Close()
		return nil, fs, err
	}
	if a.taxSet, err = tax.NewSettingsStore(filepath.Join(*dataDir, "tax.yaml"), filepath.Join(*dataDir, "backups")); err != nil {
		db.Close()
		return nil, fs, err
	}
	if a.taxYears, err = tax.NewYearStore(filepath.Join(*dataDir, "tax"), filepath.Join(*dataDir, "backups")); err != nil {
		db.Close()
		return nil, fs, err
	}
	migrateCostBasis(a.costbasis, a.taxSet, a.log)
	return a, fs, nil
}

// migrateCostBasis moves the manual lots of the old costbasis.yaml into tax.yaml, once: it
// runs only while tax.yaml doesn't exist. Lots land in the untracked wallet (edit tax.yaml
// to assign wallets); notes and per-asset overrides aren't migrated.
func migrateCostBasis(cb *costbasis.ManualStore, ts *tax.SettingsStore, log *slog.Logger) {
	if ts.Exists() {
		return
	}
	m := cb.Get()
	if len(m.Lots) == 0 {
		return
	}
	lots := make([]tax.Lot, 0, len(m.Lots))
	for _, l := range m.Lots {
		acq := l.Date.Time()
		if acq.IsZero() {
			acq = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
		}
		lots = append(lots, tax.Lot{Wallet: tax.Untracked, Asset: l.Asset, Amount: l.Amount, CostUSD: l.CostUSD, Acquired: acq})
	}
	if err := ts.Update(func(s *tax.Settings) { s.ManualLots = append(s.ManualLots, lots...) }); err != nil {
		log.Warn("could not move manual lots from costbasis.yaml to tax.yaml", "err", err)
		return
	}
	log.Warn(fmt.Sprintf("moved %d manual lots from costbasis.yaml to tax.yaml (wallet: untracked); edit tax.yaml to assign wallets", len(lots)))
	if n := len(m.Assets); n > 0 {
		log.Warn("costbasis.yaml asset overrides are not migrated to tax.yaml", "assets", n)
	}
}

// taxLoc is the configured tax timezone; an invalid one is logged and local time is used.
func (a *app) taxLoc() *time.Location {
	loc, err := a.cfg.TaxLocation()
	if err != nil {
		a.log.Warn("tax timezone not usable, using local time", "err", err)
		return time.Local
	}
	return loc
}

// syncSince is the activity backfill horizon: backfillDays before now, or the start of the
// open tax year when that is earlier, so the tax lots can always be rebuilt from the
// opening lots. With no closed year it is just now − backfillDays.
func syncSince(now time.Time, backfillDays int, years *tax.YearStore, loc *time.Location) time.Time {
	since := now.AddDate(0, 0, -backfillDays)
	last, ok := years.LastClosed()
	if open := tax.OpenYearStart(last, ok, loc); !open.IsZero() && open.Before(since) {
		return open
	}
	return since
}

// factory builds live sources from definitions with opened secrets.
func (a *app) factory(meta solana.MetaCache) sources.Factory {
	return func(d sources.Def) (core.Source, error) {
		switch d.Type {
		case sources.TypeCoinbase:
			if d.PrivateKey == "" {
				return nil, errors.New("needs its API private key: edit this source to add it")
			}
			return asSource(coinbase.New(coinbase.Config{ID: d.ID, Label: d.Label, KeyName: d.KeyName, PrivateKey: d.PrivateKey}, a.client))
		case sources.TypeKraken:
			if d.APIKey == "" || d.APISecret == "" {
				return nil, errors.New("needs its API key and secret: edit this source to add them")
			}
			return asSource(kraken.New(kraken.Config{ID: d.ID, Label: d.Label, APIKey: d.APIKey, APISecret: d.APISecret}, a.client))
		case sources.TypeBitcoin:
			return asSource(bitcoin.New(bitcoin.Config{
				ID: d.ID, Label: d.Label, Key: d.Key, Descriptor: d.Descriptor, ScriptType: d.ScriptType,
				EsploraURL: a.cfg.Bitcoin.EsploraURL, FallbackURLs: a.cfg.Bitcoin.EsploraFallbackURLs, GapLimit: a.cfg.Bitcoin.GapLimit,
				Auth: a.blockstream, ScanStore: kvScans{a.db},
			}, a.client))
		case sources.TypeSolana:
			return asSource(solana.New(solana.Config{
				ID: d.ID, Label: d.Label, Address: d.Address, JupiterVault: d.JupiterVault, RPCURL: a.cfg.Solana.RPCURL,
				IncludeStake: a.cfg.Solana.IncludeStakeAccounts, HideMints: a.cfg.Solana.HideMints, AllowMints: a.cfg.Solana.AllowMints, Overrides: a.cfg.Solana.TokenOverrides,
				DeFi: a.cfg.Solana.DeFi,
			}, a.client, meta))
		case sources.TypeManual:
			return manual.New(d.ID, d.Holdings), nil
		case sources.TypeBrokerage:
			return brokerage.New(d.ID, d.Holdings, a.tickerLookup), nil
		}
		return nil, fmt.Errorf("unknown source type %q", d.Type)
	}
}

// tickerLookup names a brokerage account's tickers with the price service.
func (a *app) tickerLookup(ctx context.Context, tickers []string) (map[string]brokerage.Security, error) {
	secs, err := a.prices.Securities(ctx, tickers)
	out := make(map[string]brokerage.Security, len(secs))
	for t, sec := range secs {
		out[t] = brokerage.Security{Name: sec.Name, Currency: sec.Currency}
	}
	return out, err
}

// asSource avoids the typed-nil trap: a nil *T with an error must become a nil interface.
func asSource[T core.Source](s T, err error) (core.Source, error) {
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (a *app) priceFunc() portfolio.PriceFunc {
	return func(ctx context.Context, reqs []portfolio.PriceReq) map[string]core.Price {
		return a.prices.Get(ctx, priceWants(reqs))
	}
}

// cachedPriceFunc prices from stored prices only, for the warm start after a restart.
func (a *app) cachedPriceFunc() portfolio.PriceFunc {
	return func(ctx context.Context, reqs []portfolio.PriceReq) map[string]core.Price {
		return a.prices.Cached(ctx, priceWants(reqs))
	}
}

func priceWants(reqs []portfolio.PriceReq) []prices.Want {
	wants := make([]prices.Want, len(reqs))
	for i, r := range reqs {
		wants[i] = prices.Want{Key: r.Key, Mint: r.Mint}
		if wants[i].Mint == "" {
			// Exchange holdings of Solana-native tokens (JUP on Kraken) get priced by mint too.
			wants[i].Mint, _ = solana.MintForSymbol(r.Key)
		}
	}
	return wants
}

func (a *app) wallets() []portfolio.Wallet {
	var out []portfolio.Wallet
	for _, e := range a.registry.Entries() {
		out = append(out, portfolio.Wallet{ID: e.Def.ID, Label: e.Def.Label, Description: e.Def.Description, Type: e.Def.Type, Kind: e.Def.Kind, Source: e.Source, BuildErr: e.BuildErr})
	}
	return out
}

// parseInterspersed lets flags appear before or after positional arguments
// ("sources import file.yaml -replace" as well as "sources import -replace file.yaml");
// the standard flag package stops at the first positional one. Positionals end up in
// fs.Args() in their original order.
func parseInterspersed(fs *flag.FlagSet, args []string) error {
	var positional []string
	for {
		if err := fs.Parse(args); err != nil {
			return err
		}
		rest := fs.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	return fs.Parse(append([]string{"--"}, positional...))
}

// activitySources lists live sources that can report transactions.
func (a *app) activitySources() []activity.Entry {
	var out []activity.Entry
	for _, e := range a.registry.Entries() {
		if src, ok := e.Source.(core.ActivitySource); ok {
			out = append(out, activity.Entry{ID: e.Def.ID, Label: e.Def.Label, Src: src})
		}
	}
	return out
}

func (a *app) walletKinds() map[string]core.Kind {
	kinds := map[string]core.Kind{}
	for _, d := range a.registry.Defs() {
		kinds[d.ID] = d.Kind
	}
	return kinds
}

// alertsEngine wires alert rules to ntfy, portfolio filters and history.
func (a *app) alertsEngine(hist *portfolio.History) *alerts.Engine {
	e := &alerts.Engine{
		Rules:  a.alertRules,
		DB:     a.db,
		Redact: a.cfg.Ntfy.RedactAmounts,
		Log:    a.log,
		Labels: func(id string) string {
			if d, ok := a.registry.Get(id); ok {
				return d.Label
			}
			return id
		},
		PortfolioLabel: func(id string) string {
			if p, ok := a.portfolios.Get(id); ok {
				return p.Name
			}
			return "All holdings"
		},
		Portfolio: func(id string, snap portfolio.Snapshot) (float64, bool) {
			if id != "" && id != "all" {
				p, ok := a.portfolios.Get(id)
				if !ok {
					return 0, false
				}
				snap = portfolios.Apply(p, snap)
			}
			return snap.TotalUSD, !snap.HasStale
		},
		History: func(ctx context.Context, id string, at time.Time) (float64, bool) {
			var inc portfolio.Include
			if id != "" && id != "all" {
				p, ok := a.portfolios.Get(id)
				if !ok {
					return 0, false
				}
				inc = portfolios.Include(p)
			}
			v, ok, err := hist.ValueAt(ctx, at, time.Hour, inc)
			return v, ok && err == nil
		},
	}
	if a.cfg.Ntfy.URL != "" {
		e.Notifier = &alerts.Ntfy{URL: a.cfg.Ntfy.URL, Token: a.cfg.NtfyToken, Client: a.client}
	}
	return e
}

// seed imports config.yaml seedSources on first run only.
func (a *app) seed() error {
	if len(a.cfg.SeedSources) == 0 || len(a.registry.Defs()) > 0 {
		return nil
	}
	raw, err := yamlfile.Encode(a.cfg.SeedSources)
	if err != nil {
		return err
	}
	var defs []sources.Def
	if err := yamlfile.Decode(raw, &defs); err != nil {
		return fmt.Errorf("seedSources: %w", err)
	}
	for _, d := range defs {
		if d.ID == "" {
			d.ID = a.registry.UniqueID(d.Label)
		}
		if _, err := a.registry.Upsert(d); err != nil {
			return fmt.Errorf("seedSources: %w", err)
		}
	}
	a.log.Info("seeded sources from config", "count", len(defs))
	return nil
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":8787", "listen address")
	a, _, err := setup(fs, args)
	if err != nil {
		return err
	}
	defer a.db.Close()
	if err := a.seed(); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hist := &portfolio.History{DB: a.db, Interval: 30 * time.Minute}
	if err := hist.Compact(ctx, time.Now(), 30*24*time.Hour); err != nil {
		a.log.Warn("history compaction failed", "err", err)
	}
	broker := portfolio.NewBroker()
	agg := &portfolio.Aggregator{
		Wallets:      a.wallets,
		Prices:       a.priceFunc(),
		CachedPrices: a.cachedPriceFunc(),
		DB:           a.db,
		StaleMax:     a.cfg.StaleMax(),
		FetchTimeout: 45 * time.Second,
		HideUnpriced: a.cfg.Solana.HideUnpriced,
		MinUSD:       a.cfg.Solana.MinUSD,
	}
	ref := &portfolio.Refresher{Agg: agg, Broker: broker, History: hist, Interval: a.cfg.RefreshInterval(), Log: a.log}

	engine := a.alertsEngine(hist)
	ref.OnSnapshot = func(snap portfolio.Snapshot) {
		go engine.OnSnapshot(context.WithoutCancel(ctx), snap) // never block the refresher on notifications
	}
	taxLoc := a.taxLoc()
	syncer := &activity.Syncer{
		Store:   a.activity,
		Sources: a.activitySources,
		Since: func() time.Time {
			return syncSince(time.Now(), a.cfg.Activity.BackfillDays, a.taxYears, taxLoc)
		},
		Interval:  a.cfg.ActivityInterval(),
		Log:       a.log,
		Valuer:    a.prices,
		SkipMints: a.cfg.Solana.HideMints,
		OnNew: func(txs []core.Tx) {
			engine.OnActivity(context.WithoutCancel(ctx), txs, a.walletKinds())
		},
	}
	a.registry.OnChange(func() { ref.Kick(); syncer.Kick() })
	go ref.Run(ctx)
	go syncer.Run(ctx)

	h := server.New(server.Deps{
		Cfg: a.cfg, DataDir: a.dataDir, HTTPClient: &http.Client{Timeout: 30 * time.Second}, Version: version, Log: a.log, Box: a.box,
		Registry: a.registry, Portfolios: a.portfolios, Refresher: ref,
		Activity: a.activity, Syncer: syncer, CostBasis: a.costbasis, AlertRules: a.alertRules, Alerts: engine, DB: a.db, Broker: broker, History: hist, Prices: a.priceFunc(), Historical: a.prices,
		TaxSettings: a.taxSet, TaxYears: a.taxYears, TaxLoc: taxLoc,
		DetectBitcoin:          bitcoin.Detect,
		WalletFileToDescriptor: bitcoin.DescriptorFromWalletFile,
		ParseLedgerExport:      bitcoin.ParseLedgerExport,
		Blockstream:            a.blockstream,
	})
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("CryptKeep listening", "addr", *addr, "sources", len(a.registry.Defs()), "auth", a.cfg.Password != "", "data", a.dataDir)
		errCh <- srv.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
		a.log.Info("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx) // SSE streams end when their request contexts are cancelled
	}
	return nil
}

func sourcesCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: sources export [-o file] [-secrets] | sources import FILE [-replace]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("sources "+sub, flag.ExitOnError)
	switch sub {
	case "export":
		out := fs.String("o", "", "output file (default stdout)")
		withSecrets := fs.Bool("secrets", false, "include sealed secrets (only re-importable with the same key; always on when CRYPTKEEP_SECRET_KEY is set)")
		a, _, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		raw, err := a.registry.Export(*withSecrets || a.cfg.EncryptExports())
		if err != nil {
			return err
		}
		if raw, err = a.sealExport(raw); err != nil {
			return err
		}
		if *out == "" {
			_, err = os.Stdout.Write(raw)
			return err
		}
		return os.WriteFile(*out, raw, 0o600)
	case "import":
		replace := fs.Bool("replace", false, "remove sources not present in the file")
		a, fs, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		if fs.NArg() != 1 {
			return errors.New("usage: sources import FILE [-replace]")
		}
		raw, err := a.readImport(fs.Arg(0))
		if err != nil {
			return err
		}
		f, err := sources.ParseFile(raw)
		if err != nil {
			return fmt.Errorf("%s: %w", fs.Arg(0), err)
		}
		res, err := a.registry.Import(f, *replace)
		if err != nil {
			return err
		}
		for _, id := range res.Removed {
			if _, err := a.portfolios.RemoveWallet(id); err != nil {
				return err
			}
			if err := a.activity.DeleteSource(context.Background(), id); err != nil {
				return err
			}
		}
		fmt.Printf("imported: %d added, %d replaced, %d unchanged, %d removed\n", res.Added, res.Replaced, res.Unchanged, len(res.Removed))
		return nil
	}
	return fmt.Errorf("unknown sources command %q", sub)
}

func portfoliosCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: portfolios export [-o file] [-id ID] | portfolios import FILE [-replace]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("portfolios "+sub, flag.ExitOnError)
	switch sub {
	case "export":
		out := fs.String("o", "", "output file (default stdout)")
		id := fs.String("id", "", "export one portfolio (default: all)")
		withAlerts := fs.Bool("alerts", true, "include alert rules when exporting all")
		a, _, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		raw, err := a.portfolios.Export(*id)
		if err != nil {
			return err
		}
		if *id == "" && *withAlerts {
			if raw, err = portfolios.WithAlerts(raw, a.alertRules.List()); err != nil {
				return err
			}
		}
		if raw, err = a.sealExport(raw); err != nil {
			return err
		}
		if *out == "" {
			_, err = os.Stdout.Write(raw)
			return err
		}
		return os.WriteFile(*out, raw, 0o600)
	case "import":
		replace := fs.Bool("replace", false, "replace the whole list instead of merging")
		a, fs, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		if fs.NArg() != 1 {
			return errors.New("usage: portfolios import FILE [-replace]")
		}
		raw, err := a.readImport(fs.Arg(0))
		if err != nil {
			return err
		}
		mode := portfolios.ImportMerge
		if *replace {
			mode = portfolios.ImportReplace
		}
		res, err := a.portfolios.Import(raw, mode)
		if err != nil {
			return fmt.Errorf("%s: %w", fs.Arg(0), err)
		}
		fmt.Printf("imported: %d added, %d replaced, %d unchanged, %d removed\n", res.Added, res.Replaced, res.Unchanged, res.Removed)
		if f, err := portfolios.Parse(raw); err == nil && len(f.Alerts) > 0 {
			ar, err := a.alertRules.Import(f.Alerts, *replace)
			if err != nil {
				return fmt.Errorf("alert rules: %w", err)
			}
			fmt.Printf("alert rules: %d added, %d replaced, %d unchanged\n", ar.Added, ar.Replaced, ar.Unchanged)
		}
		return nil
	}
	return fmt.Errorf("unknown portfolios command %q", sub)
}

// backupCmd exports or restores everything (sources, portfolios, alert rules, cost basis).
func backupCmd(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: backup export [-o file] [-secrets] | backup import FILE [-replace]")
	}
	sub, rest := args[0], args[1:]
	fs := flag.NewFlagSet("backup "+sub, flag.ExitOnError)
	switch sub {
	case "export":
		out := fs.String("o", "", "output file (default stdout)")
		withSecrets := fs.Bool("secrets", false, "include sealed exchange secrets (only restorable with the same key; always on when CRYPTKEEP_SECRET_KEY is set)")
		a, _, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		raw, err := backup.Export(a.backupStores(), *withSecrets || a.cfg.EncryptExports())
		if err != nil {
			return err
		}
		if raw, err = a.sealExport(raw); err != nil {
			return err
		}
		if *out == "" {
			_, err = os.Stdout.Write(raw)
			return err
		}
		return os.WriteFile(*out, raw, 0o600)
	case "import":
		replace := fs.Bool("replace", false, "each section in the file replaces the current one")
		a, fs, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		if fs.NArg() != 1 {
			return errors.New("usage: backup import FILE [-replace]")
		}
		raw, err := a.readImport(fs.Arg(0))
		if err != nil {
			return err
		}
		res, err := backup.Import(a.backupStores(), raw, *replace)
		fmt.Println(res.Summary())
		if err != nil {
			return fmt.Errorf("%s: %w", fs.Arg(0), err)
		}
		return nil
	}
	return fmt.Errorf("unknown backup command %q", sub)
}

// sealExport encrypts a YAML export as a whole when CRYPTKEEP_SECRET_KEY is set.
func (a *app) sealExport(raw []byte) ([]byte, error) {
	if !a.cfg.EncryptExports() {
		return raw, nil
	}
	return a.box.SealFile(raw)
}

// readImport reads a file to import, decrypting it if it is an encrypted export.
func (a *app) readImport(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if raw, err = a.box.OpenFile(raw); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return raw, nil
}

func (a *app) backupStores() backup.Stores {
	return backup.Stores{
		Sources: a.registry, Portfolios: a.portfolios, Alerts: a.alertRules, CostBasis: a.costbasis,
		Tax: a.taxSet, TaxYears: a.taxYears,
		OnSourceRemoved: func(id string) {
			a.portfolios.RemoveWallet(id)
			a.activity.DeleteSource(context.Background(), id)
		},
	}
}

func healthcheck(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:8787", "server address")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + *addr + "/healthz")
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unhealthy: HTTP %d", resp.StatusCode)
	}
	return nil
}

// kvScans keeps Bitcoin wallets' address scans in the kv table, so a restart doesn't read
// every address again.
type kvScans struct{ db *store.DB }

func (k kvScans) LoadScan(ctx context.Context, walletID string) ([]byte, error) {
	var raw json.RawMessage
	if ok, err := k.db.GetJSON(ctx, server.BitcoinScanKVPrefix+walletID, &raw); err != nil || !ok {
		return nil, err
	}
	return raw, nil
}

func (k kvScans) SaveScan(ctx context.Context, walletID string, data []byte) error {
	return k.db.PutJSON(ctx, server.BitcoinScanKVPrefix+walletID, json.RawMessage(data))
}

// blockstreamAuth returns the Blockstream API key holder, loaded from the environment when
// BLOCKSTREAM_CLIENT_ID is set, else from the key saved in Settings.
func blockstreamAuth(cfg config.Config, db *store.DB, box *secrets.Box, c *httpx.Client, log *slog.Logger) *bitcoin.BlockstreamAuth {
	auth := bitcoin.NewBlockstreamAuth(c)
	if cfg.BlockstreamClientID != "" {
		auth.SetCredentials(cfg.BlockstreamClientID, cfg.BlockstreamClientSecret)
		return auth
	}
	k, ok, err := server.LoadBlockstreamKey(context.Background(), db, box)
	if err != nil {
		log.Warn("open blockstream key", "err", err)
	}
	if ok {
		auth.SetCredentials(k.ID, k.Secret)
	}
	return auth
}

// coinGeckoKey returns the CoinGecko demo key: the environment's when set, else the one saved
// in Settings (sealed in the kv table). The Settings handler can't reach this closure, so the
// stored value is cached for a minute.
func coinGeckoKey(env string, db *store.DB, box *secrets.Box) func() string {
	var (
		mu      sync.Mutex
		cached  string
		fetched time.Time
	)
	return func() string {
		if env != "" {
			return env
		}
		mu.Lock()
		defer mu.Unlock()
		if !fetched.IsZero() && time.Since(fetched) < time.Minute {
			return cached
		}
		cached, fetched = "", time.Now()
		var sealed string
		if ok, err := db.GetJSON(context.Background(), server.PricesKeyKV, &sealed); err == nil && ok {
			if plain, err := box.Open(sealed); err == nil {
				cached = plain
			}
		}
		return cached
	}
}
