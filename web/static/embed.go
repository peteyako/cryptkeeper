// Package static embeds the web assets: design tokens, component styles, the vendored
// Datastar bundle, the chart hover script, self-hosted fonts, icons and the PWA manifest. Serve FS under /static/,
// and serve manifest.webmanifest at /manifest.webmanifest.
package static

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
)

//go:embed tokens.css app.css datastar.js chart.js fonts icons manifest.webmanifest
var FS embed.FS

// Version is a short content hash of every embedded file. Pages reference assets as
// /static/<file>?v=<Version>, so browsers fetch new CSS/JS after an upgrade instead of
// serving a stale cached copy, while unchanged assets can be cached aggressively.
var Version = func() string {
	h := sha256.New()
	_ = fs.WalkDir(FS, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		raw, err := FS.ReadFile(path)
		if err != nil {
			return err
		}
		h.Write([]byte(path))
		h.Write(raw)
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:10]
}()
