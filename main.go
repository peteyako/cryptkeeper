// Command crypto-portfolio is Vault: a self-hosted, read-only crypto portfolio monitor.
//
//	crypto-portfolio serve [-config config.yaml] [-data ./data] [-env .env] [-addr :8787]
//	crypto-portfolio portfolios export [-o file.yaml] [-id ID]
//	crypto-portfolio portfolios import file.yaml [-replace]
//	crypto-portfolio sources export [-o file.yaml] [-secrets]
//	crypto-portfolio sources import file.yaml [-replace]
//	crypto-portfolio healthcheck [-addr 127.0.0.1:8787]
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"crypto-portfolio/internal/config"
	"crypto-portfolio/internal/core"
	"crypto-portfolio/internal/httpx"
	"crypto-portfolio/internal/portfolio"
	"crypto-portfolio/internal/portfolios"
	"crypto-portfolio/internal/prices"
	"crypto-portfolio/internal/secrets"
	"crypto-portfolio/internal/server"
	"crypto-portfolio/internal/sources"
	"crypto-portfolio/internal/sources/bitcoin"
	"crypto-portfolio/internal/sources/coinbase"
	"crypto-portfolio/internal/sources/kraken"
	"crypto-portfolio/internal/sources/manual"
	"crypto-portfolio/internal/sources/solana"
	"crypto-portfolio/internal/store"
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
	case "healthcheck":
		err = healthcheck(args)
	case "version":
		fmt.Println(version)
	default:
		err = fmt.Errorf("unknown command %q (serve, portfolios, sources, healthcheck, version)", cmd)
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
	client     *httpx.Client
	registry   *sources.Registry
	portfolios *portfolios.Store
	prices     *prices.Service
}

// setup loads config, opens the database and the sources registry.
func setup(fs *flag.FlagSet, args []string) (*app, *flag.FlagSet, error) {
	cfgPath := fs.String("config", "config.yaml", "server settings file")
	dataDir := fs.String("data", "data", "data directory (sources, portfolios, database, backups)")
	envPath := fs.String("env", ".env", "optional KEY=VALUE file for secrets")
	if err := fs.Parse(args); err != nil {
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
	db, err := store.Open(filepath.Join(*dataDir, "vault.db"))
	if err != nil {
		return nil, fs, err
	}
	a := &app{cfg: cfg, dataDir: *dataDir, log: log, db: db, client: httpx.New()}
	a.prices = prices.New(a.client, db, prices.Options{TTL: time.Minute, StaleMax: cfg.StaleMax()})
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
	return a, fs, nil
}

// factory builds live sources from definitions with opened secrets.
func (a *app) factory(meta solana.MetaCache) sources.Factory {
	return func(d sources.Def) (core.Source, error) {
		switch d.Type {
		case sources.TypeCoinbase:
			return asSource(coinbase.New(coinbase.Config{ID: d.ID, Label: d.Label, KeyName: d.KeyName, PrivateKey: d.PrivateKey}, a.client))
		case sources.TypeKraken:
			return asSource(kraken.New(kraken.Config{ID: d.ID, Label: d.Label, APIKey: d.APIKey, APISecret: d.APISecret}, a.client))
		case sources.TypeBitcoin:
			return asSource(bitcoin.New(bitcoin.Config{
				ID: d.ID, Label: d.Label, Key: d.Key, Descriptor: d.Descriptor, ScriptType: d.ScriptType,
				EsploraURL: a.cfg.Bitcoin.EsploraURL, GapLimit: a.cfg.Bitcoin.GapLimit,
			}, a.client))
		case sources.TypeSolana:
			return asSource(solana.New(solana.Config{
				ID: d.ID, Label: d.Label, Address: d.Address, RPCURL: a.cfg.Solana.RPCURL,
				IncludeStake: a.cfg.Solana.IncludeStakeAccounts, HideMints: a.cfg.Solana.HideMints, AllowMints: a.cfg.Solana.AllowMints, Overrides: a.cfg.Solana.TokenOverrides,
			}, a.client, meta))
		case sources.TypeManual:
			return manual.New(d.ID, d.Holdings), nil
		}
		return nil, fmt.Errorf("unknown source type %q", d.Type)
	}
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
		wants := make([]prices.Want, len(reqs))
		for i, r := range reqs {
			wants[i] = prices.Want{Key: r.Key, Mint: r.Mint}
			if wants[i].Mint == "" {
				// Exchange holdings of Solana-native tokens (JUP on Kraken) get priced by mint too.
				wants[i].Mint, _ = solana.MintForSymbol(r.Key)
			}
		}
		return a.prices.Get(ctx, wants)
	}
}

func (a *app) wallets() []portfolio.Wallet {
	var out []portfolio.Wallet
	for _, e := range a.registry.Entries() {
		out = append(out, portfolio.Wallet{ID: e.Def.ID, Label: e.Def.Label, Type: e.Def.Type, Kind: e.Def.Kind, Source: e.Source, BuildErr: e.BuildErr})
	}
	return out
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
		DB:           a.db,
		StaleMax:     a.cfg.StaleMax(),
		FetchTimeout: 45 * time.Second,
		HideUnpriced: a.cfg.Solana.HideUnpriced,
		MinUSD:       a.cfg.Solana.MinUSD,
	}
	ref := &portfolio.Refresher{Agg: agg, Broker: broker, History: hist, Interval: a.cfg.RefreshInterval(), Log: a.log}
	a.registry.OnChange(ref.Kick)
	go ref.Run(ctx)

	h := server.New(server.Deps{
		Cfg: a.cfg, DataDir: a.dataDir, Version: version, Log: a.log,
		Registry: a.registry, Portfolios: a.portfolios, Refresher: ref, Broker: broker, History: hist, Prices: a.priceFunc(),
		DetectBitcoin:          bitcoin.Detect,
		WalletFileToDescriptor: bitcoin.DescriptorFromWalletFile,
	})
	srv := &http.Server{Addr: *addr, Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errCh := make(chan error, 1)
	go func() {
		a.log.Info("Vault listening", "addr", *addr, "sources", len(a.registry.Defs()), "auth", a.cfg.Password != "", "data", a.dataDir)
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
		withSecrets := fs.Bool("secrets", false, "include sealed secrets (only re-importable with the same key)")
		a, _, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		raw, err := a.registry.Export(*withSecrets)
		if err != nil {
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
		raw, err := os.ReadFile(fs.Arg(0))
		if err != nil {
			return err
		}
		var f sources.File
		if err := yamlfile.Decode(raw, &f); err != nil {
			return fmt.Errorf("%s: %w", fs.Arg(0), err)
		}
		keep := map[string]bool{}
		for _, d := range f.Sources {
			keep[d.ID] = true
			if _, err := a.registry.Upsert(d); err != nil {
				return err
			}
		}
		if *replace {
			for _, d := range a.registry.Defs() {
				if !keep[d.ID] {
					if err := a.registry.Delete(d.ID); err != nil {
						return err
					}
				}
			}
		}
		fmt.Printf("imported %d sources\n", len(f.Sources))
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
		a, _, err := setup(fs, rest)
		if err != nil {
			return err
		}
		defer a.db.Close()
		raw, err := a.portfolios.Export(*id)
		if err != nil {
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
		raw, err := os.ReadFile(fs.Arg(0))
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
		return nil
	}
	return fmt.Errorf("unknown portfolios command %q", sub)
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
