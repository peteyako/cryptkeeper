package backup

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"crypto-portfolio/internal/tax"
)

func writeOffsOnly(asset string) tax.Settings {
	return tax.Settings{WriteOffs: []tax.WriteOff{{Wallet: "wallet-a", Asset: asset, Amount: 2, Date: time.Date(2024, 6, 1, 0, 0, 0, 0, time.UTC), ProceedsUSD: 0, Note: "liquidated"}}}
}

func TestBackupKeepsWriteOffs(t *testing.T) {
	src, _ := newStores(t)
	if err := src.Tax.Set(writeOffsOnly("AAA")); err != nil {
		t.Fatal(err)
	}
	raw, err := Export(src, false)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "writeOffs") {
		t.Fatalf("export lost the write-offs:\n%s", raw)
	}

	// Merge: adds the backup's write-off, keeps the local one, and doesn't duplicate.
	dst, _ := newStores(t)
	if err := dst.Tax.Set(writeOffsOnly("BBB")); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := Import(dst, raw, false); err != nil {
			t.Fatal(err)
		}
	}
	if got := dst.Tax.Get().WriteOffs; len(got) != 2 || got[0].Asset != "BBB" || got[1].Asset != "AAA" {
		t.Fatalf("merge write-offs = %+v", got)
	}

	// Replace: exactly the backup's.
	if _, err := Import(dst, raw, true); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.Tax.Get().WriteOffs, src.Tax.Get().WriteOffs; !reflect.DeepEqual(got, want) {
		t.Fatalf("replace write-offs\n got %+v\nwant %+v", got, want)
	}
}
