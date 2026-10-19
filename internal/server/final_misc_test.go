package server

import (
	"bytes"
	"mime/multipart"
	"reflect"
	"strings"
	"testing"
	"time"

	"cryptkeeper/internal/tax"
)

func fmYear() tax.Year {
	var d []tax.Disposal
	for i, asset := range []string{"AAA", "BBB", "CCC"} {
		d = append(d, tax.Disposal{
			Sold: time.Date(2024, time.Month(3+i), 1, 0, 0, 0, 0, time.UTC), Acquired: time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC),
			Asset: asset, Amount: 1, ProceedsUSD: 100, CostUSD: 60, GainUSD: 40, Wallet: "wallet-a", Term: tax.ShortTerm, TxID: "tx-" + asset,
		})
	}
	return tax.Year{Year: 2024, Source: "cryptkeeper", ClosedAt: time.Date(2025, 1, 5, 0, 0, 0, 0, time.UTC), Summary: tax.Summarize(d, nil), Disposals: d}
}

// fmRestore posts raw as a backup file in the given mode and returns the response body.
func fmRestore(e *env, raw []byte, mode string) string {
	e.t.Helper()
	body, hdr := multipartFile(e.t, "file", "backup.yaml", raw, map[string]string{"mode": mode})
	_, out := e.do("POST", "/backup/import", body, hdr)
	return out
}

func TestRestoreRefusesOversizeFile(t *testing.T) {
	e := newEnv(t, "")
	limit := backupRestoreLimit
	backupRestoreLimit = 1 << 20
	t.Cleanup(func() { backupRestoreLimit = limit })
	big := bytes.Repeat([]byte("# padding\n"), (1<<20)/10+10)
	out := fmRestore(e, big, "merge")
	if !strings.Contains(out, "too large") || !strings.Contains(out, "1 MB") {
		t.Fatalf("oversize restore not refused clearly: %.300s", out)
	}
}

func TestRestoreRefusesTruncatedYear(t *testing.T) {
	e := newEnv(t, "")
	y := fmYear()
	if err := e.taxYears.Save(y); err != nil {
		t.Fatal(err)
	}
	_, file := e.do("GET", "/backup/export", nil, nil)
	// Cut the file at the start of the third disposal: valid YAML, but rows are missing.
	first := strings.Index(file, "- sold:")
	if first < 0 {
		t.Fatalf("no disposals in export:\n%s", file)
	}
	third := first
	for i := 0; i < 2; i++ {
		next := strings.Index(file[third+1:], "- sold:")
		if next < 0 {
			t.Fatalf("expected 3 disposals:\n%s", file)
		}
		third += 1 + next
	}
	lineStart := strings.LastIndex(file[:third], "\n") + 1
	cut := file[:lineStart]

	before, _ := e.taxYears.Get(2024)
	out := fmRestore(e, []byte(cut), "replace")
	if !strings.Contains(out, "Nothing was restored") {
		t.Fatalf("truncated backup not refused: %.400s", out)
	}
	if after, ok := e.taxYears.Get(2024); !ok || !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused restore changed the year\n before %+v\n after  %+v", before, after)
	}
}

func TestCSVImportRefreshesTaxSection(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("source", "coinbase")
	fw, _ := mw.CreateFormFile("file", "coinbase.csv")
	fw.Write([]byte("ID,Timestamp,Transaction Type,Asset,Quantity Transacted,Price Currency,Price at Transaction,Subtotal,Total (inclusive of fees and/or spread),Fees and/or Spread,Notes\n" +
		"fm-1,2025-01-02T10:00:00Z,Buy,ETH,1,USD,$3000.00,$3000.00,$3010.00,$10.00,Bought 1 ETH\n"))
	mw.Close()
	_, out := e.do("POST", "/reports/import", &buf, ds(map[string]string{"Content-Type": mw.FormDataContentType()}))
	if !strings.Contains(out, "Imported 1 transactions") {
		t.Fatalf("csv import: %.400s", out)
	}
	if !strings.Contains(out, `id="tax-section"`) {
		t.Fatalf("the Tax section was not patched after the import: %.600s", out)
	}
}

func TestCSVImportRefusesOversizeFile(t *testing.T) {
	e := newEnv(t, "")
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("source", "coinbase")
	fw, _ := mw.CreateFormFile("file", "coinbase.csv")
	fw.Write(bytes.Repeat([]byte("x,y\n"), (9<<20)/4))
	mw.Close()
	_, out := e.do("POST", "/reports/import", &buf, ds(map[string]string{"Content-Type": mw.FormDataContentType()}))
	if !strings.Contains(out, "too large") {
		t.Fatalf("oversize csv not refused: %.300s", out)
	}
}
