package server

import (
	"bytes"
	"strings"
	"testing"

	"cryptkeeper/internal/alerts"
	"cryptkeeper/internal/portfolios"
	"cryptkeeper/internal/secrets"
	"cryptkeeper/internal/sources"
	"cryptkeeper/internal/sources/manual"
)

// withExportKey is an install where the user set CRYPTKEEPER_SECRET_KEY (d.Box holds the key).
func withExportKey(d *Deps) { d.Cfg.SecretKey = "c2V0" }

func TestEncryptedBackupRoundTrip(t *testing.T) {
	e := newEnv(t, "", withExportKey)
	e.reg.Upsert(sources.Def{ID: "kraken", Type: sources.TypeKraken, Label: "Kraken", APIKey: "pub-key", APISecret: "very-secret"})
	e.reg.Upsert(sources.Def{ID: "kids", Type: sources.TypeManual, Label: "Kids", Holdings: []manual.Holding{{Asset: "SOL", Amount: 3}}})
	e.ps.Upsert(portfolios.Portfolio{ID: "test", Name: "Test", Wallets: portfolios.Selector{IDs: []string{"kids"}}, Assets: portfolios.AllSelector()})

	resp, file := e.do("GET", "/backup/export", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(file, "cryptkeeper: encrypted") {
		t.Fatalf("export: %d\n%s", resp.StatusCode, file)
	}
	for _, leak := range []string{"very-secret", "Kids", "sources:", "portfolios:"} {
		if strings.Contains(file, leak) {
			t.Fatalf("encrypted export leaks %q:\n%s", leak, file)
		}
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="cryptkeeper-backup-encrypted-`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// Remove everything, then restore: exchange secrets come back with the rest.
	e.ps.Delete("test")
	e.do("POST", "/sources/kids/delete", nil, ds(nil))
	e.do("POST", "/sources/kraken/delete", nil, ds(nil))
	body, hdr := multipartFile(t, "file", "cryptkeeper-backup-encrypted.yaml", []byte(file), map[string]string{"mode": "merge"})
	if _, out := e.do("POST", "/backup/import", body, hdr); !strings.Contains(out, "portfolios 1 added") {
		t.Fatalf("restore: %.500s", out)
	}
	d, ok := e.reg.Get("kraken")
	if !ok {
		t.Fatal("kraken not restored")
	}
	if open, err := e.reg.Open(d); err != nil || open.APISecret != "very-secret" {
		t.Fatalf("kraken secret not restored: %q %v", open.APISecret, err)
	}
	if _, ok := e.ps.Get("test"); !ok {
		t.Fatal("portfolio not restored")
	}
}

func TestEncryptedPortfoliosRoundTrip(t *testing.T) {
	e := newEnv(t, "", withExportKey)
	e.ps.Upsert(portfolios.Portfolio{ID: "lt", Name: "Long-term", Wallets: portfolios.AllSelector(), Assets: portfolios.AllSelector()})
	e.rules.Upsert(alerts.Rule{ID: "btc-high", Type: alerts.TypePrice, Asset: "BTC", Above: 100000})

	resp, file := e.do("GET", "/portfolios/export", nil, nil)
	if !strings.Contains(file, "cryptkeeper: encrypted") || strings.Contains(file, "Long-term") || strings.Contains(file, "btc-high") {
		t.Fatalf("export all is not encrypted:\n%s", file)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, `filename="portfolios-encrypted-`) {
		t.Fatalf("Content-Disposition = %q", cd)
	}
	if _, one := e.do("GET", "/portfolios/lt/export", nil, nil); !strings.Contains(one, "cryptkeeper: encrypted") {
		t.Fatalf("single-portfolio export is not encrypted:\n%s", one)
	}

	e.ps.Delete("lt")
	e.rules.Delete("btc-high")
	body, hdr := multipartFile(t, "file", "portfolios-encrypted.yaml", []byte(file), map[string]string{"mode": "merge"})
	if _, out := e.do("POST", "/portfolios/import", body, hdr); !strings.Contains(out, "Alert rules: 1 added") {
		t.Fatalf("import: %.400s", out)
	}
	if _, ok := e.ps.Get("lt"); !ok {
		t.Fatal("portfolio not restored")
	}
}

func TestImportEncryptedFileWithWrongKey(t *testing.T) {
	e := newEnv(t, "")
	other, _ := secrets.NewBox(bytes.Repeat([]byte{9}, 32))
	file, _ := other.SealFile([]byte("version: 1\nportfolios:\n  - {id: lt, name: Long-term}\n"))

	body, hdr := multipartFile(t, "file", "b.yaml", file, map[string]string{"mode": "merge"})
	if _, out := e.do("POST", "/backup/import", body, hdr); !strings.Contains(out, "Nothing was restored") || !strings.Contains(out, "different key") {
		t.Fatalf("backup import: %.400s", out)
	}
	body, hdr = multipartFile(t, "file", "p.yaml", file, map[string]string{"mode": "merge"})
	if _, out := e.do("POST", "/portfolios/import", body, hdr); !strings.Contains(out, "Nothing was imported") || !strings.Contains(out, "different key") {
		t.Fatalf("portfolios import: %.400s", out)
	}
	if _, ok := e.ps.Get("lt"); ok {
		t.Fatal("a file that can't be decrypted must change nothing")
	}
}

func TestExportsStayPlainWithoutKey(t *testing.T) {
	e := newEnv(t, "")
	e.ps.Upsert(portfolios.Portfolio{ID: "lt", Name: "Long-term", Wallets: portfolios.AllSelector(), Assets: portfolios.AllSelector()})
	for _, path := range []string{"/backup/export", "/portfolios/export"} {
		if _, file := e.do("GET", path, nil, nil); strings.Contains(file, "cryptkeeper: encrypted") || !strings.Contains(file, "Long-term") {
			t.Fatalf("%s should be plain YAML:\n%s", path, file)
		}
	}
}

func TestPagesSayWhenExportsAreEncrypted(t *testing.T) {
	plain := newEnv(t, "")
	if _, page := plain.do("GET", "/settings", nil, nil); !strings.Contains(page, "With encrypted secrets") || strings.Contains(page, "Export encrypted") {
		t.Fatal("without a key, Settings offers the plain export and the with-secrets one")
	}
	e := newEnv(t, "", withExportKey)
	_, page := e.do("GET", "/settings", nil, nil)
	if !strings.Contains(page, "Export encrypted") || !strings.Contains(page, "encrypted with your") || strings.Contains(page, "With encrypted secrets") {
		t.Fatal("with a key, Settings has one encrypted export and says so")
	}
	if _, page := e.do("GET", "/portfolios", nil, nil); !strings.Contains(page, "encrypted with your") {
		t.Fatal("the portfolios export says it is encrypted")
	}
}
