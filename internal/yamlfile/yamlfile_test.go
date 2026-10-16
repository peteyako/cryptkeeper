package yamlfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDecodeRejectsUnknownFieldsWithLine(t *testing.T) {
	var v struct {
		Name string `yaml:"name"`
	}
	err := Decode([]byte("name: a\nnmae: b\n"), &v)
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("want line-numbered error, got %v", err)
	}
	if err := Decode(nil, &v); err != nil {
		t.Fatalf("empty input: %v", err)
	}
}

func TestWriteAtomicBackupsAndPrune(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "portfolios.yaml")
	backups := filepath.Join(dir, "backups")
	for i := range 5 {
		if err := WriteAtomic(path, []byte{byte('a' + i)}, backups, 3); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Millisecond) // distinct backup timestamps
	}
	got, _ := os.ReadFile(path)
	if string(got) != "e" {
		t.Fatalf("content %q", got)
	}
	list, err := ListBackups(backups, "portfolios")
	if err != nil || len(list) != 3 {
		t.Fatalf("want 3 backups, got %d %v", len(list), err)
	}
	newest, err := ReadBackup(backups, "portfolios", list[0].Name)
	if err != nil || string(newest) != "d" {
		t.Fatalf("newest backup %q %v", newest, err)
	}
	if _, err := ReadBackup(backups, "portfolios", "../portfolios.yaml"); err == nil {
		t.Fatal("path traversal must be rejected")
	}
}
