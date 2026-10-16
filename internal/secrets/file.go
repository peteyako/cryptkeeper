package secrets

import (
	"errors"
	"fmt"

	"gopkg.in/yaml.v3"
)

// fileTag marks a whole export (a backup, sources or portfolios file) encrypted by SealFile.
const (
	fileTag     = "encrypted"
	fileVersion = 1
)

type envelope struct {
	CryptKeep string `yaml:"cryptkeep"`
	Version   int    `yaml:"version"`
	Data      string `yaml:"data"`
}

// SealFile encrypts a whole export into a small YAML file that OpenFile recognizes:
//
//	cryptkeep: encrypted
//	version: 1
//	data: "enc:v1:…"
func (b *Box) SealFile(plain []byte) ([]byte, error) {
	data, err := b.Seal(string(plain))
	if err != nil {
		return nil, err
	}
	return fmt.Appendf(nil, "# CryptKeep export, encrypted with CRYPTKEEP_SECRET_KEY. Import it into a CryptKeep\n"+
		"# that has the same key.\ncryptkeep: %s\nversion: %d\ndata: %q\n", fileTag, fileVersion, data), nil
}

// OpenFile returns the export inside a SealFile envelope. Anything else, including a file
// that isn't valid YAML, is returned unchanged so plain exports keep importing and their
// own parser reports what is wrong. A nil Box opens only plain files.
func (b *Box) OpenFile(raw []byte) ([]byte, error) {
	var env envelope
	if yaml.Unmarshal(raw, &env) != nil || env.CryptKeep != fileTag {
		return raw, nil
	}
	if env.Version != fileVersion {
		return nil, fmt.Errorf("encrypted file version %d is not supported (this build reads version %d)", env.Version, fileVersion)
	}
	if !IsSealed(env.Data) {
		return nil, errors.New("encrypted file has no encrypted data")
	}
	if b == nil {
		return nil, errors.New("the file is encrypted: set CRYPTKEEP_SECRET_KEY to the key it was exported with")
	}
	plain, err := b.Open(env.Data)
	if err != nil {
		return nil, errors.New("the file is encrypted with a different key: set CRYPTKEEP_SECRET_KEY to the key it was exported with")
	}
	return []byte(plain), nil
}
