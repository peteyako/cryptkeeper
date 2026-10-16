// Package yamlfile reads and writes the human-editable YAML stores (sources, portfolios,
// alert rules): strict decoding with line-numbered errors, atomic writes, and timestamped
// backups before every overwrite.
package yamlfile

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Decode strictly decodes raw into out: unknown fields are errors (with line numbers).
func Decode(raw []byte, out any) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// Encode marshals v with 2-space indentation.
func Encode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// WriteAtomic writes data to path via a temp file, fsync and rename. If path already
// exists, its current content is first copied to backupDir as <base>-YYYYMMDD-HHMMSS.yaml,
// keeping only the newest keep backups for that base name.
func WriteAtomic(path string, data []byte, backupDir string, keep int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if backupDir != "" {
		if err := Backup(path, backupDir, keep); err != nil {
			return fmt.Errorf("backup: %w", err)
		}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// Backup copies path (if it exists) into backupDir and prunes old backups.
func Backup(path, backupDir string, keep int) error {
	cur, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return err
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	name := fmt.Sprintf("%s-%s.yaml", base, time.Now().UTC().Format("20060102-150405.000"))
	if err := os.WriteFile(filepath.Join(backupDir, name), cur, 0o600); err != nil {
		return err
	}
	return prune(backupDir, base, keep)
}

// BackupInfo describes one backup file.
type BackupInfo struct {
	Name string
	Time time.Time
	Size int64
}

// ListBackups returns backups for base (e.g. "portfolios"), newest first.
func ListBackups(backupDir, base string) ([]BackupInfo, error) {
	entries, err := os.ReadDir(backupDir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []BackupInfo
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasPrefix(n, base+"-") || !strings.HasSuffix(n, ".yaml") {
			continue
		}
		ts := strings.TrimSuffix(strings.TrimPrefix(n, base+"-"), ".yaml")
		t, err := time.Parse("20060102-150405.000", ts)
		if err != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, BackupInfo{Name: n, Time: t, Size: info.Size()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.After(out[j].Time) })
	return out, nil
}

// ReadBackup returns the content of a named backup. The name must be one returned by
// ListBackups for base, which rules out path traversal.
func ReadBackup(backupDir, base, name string) ([]byte, error) {
	list, err := ListBackups(backupDir, base)
	if err != nil {
		return nil, err
	}
	for _, b := range list {
		if b.Name == name {
			return os.ReadFile(filepath.Join(backupDir, b.Name))
		}
	}
	return nil, fmt.Errorf("unknown backup %q", name)
}

func prune(backupDir, base string, keep int) error {
	if keep <= 0 {
		return nil
	}
	list, err := ListBackups(backupDir, base)
	if err != nil {
		return err
	}
	for _, b := range list[min(keep, len(list)):] {
		if err := os.Remove(filepath.Join(backupDir, b.Name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
