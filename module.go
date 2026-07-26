package jsingo

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
)

// Mod is a unit of JavaScript: an entrypoint, its supporting files, and the
// npm dependencies they import.
//
// Build one with [Module] from an embedded filesystem:
//
//	//go:embed js/*.ts js/package.json
//	var jsFS embed.FS
//
//	var article = jsingo.Module(jsFS, "js/article.ts")
//
// Embed with explicit patterns, never a bare directory. A directory embed
// recurses and skips only entries prefixed with "." or "_", so a node_modules
// left beside the sources is pulled into the binary silently - tens of
// megabytes, no error, no warning.
type Mod struct {
	name  string
	entry string
	fsys  fs.FS
	err   error
}

// Module declares a JavaScript module rooted at fsys with the given entry.
//
// The module's name is derived from the entry filename and is what qualifies
// its exports when several modules are loaded together ("article:parse").
//
// Errors are deferred to [New] rather than returned here, so modules can be
// declared as package-level variables in the ordinary way.
func Module(fsys fs.FS, entry string) *Mod {
	m := &Mod{fsys: fsys, entry: path.Clean(entry)}

	switch {
	case fsys == nil:
		m.err = fmt.Errorf("jsingo: Module(%q): nil filesystem", entry)
	case entry == "":
		m.err = fmt.Errorf("jsingo: Module: empty entry path")
	case path.IsAbs(m.entry) || strings.HasPrefix(m.entry, ".."):
		m.err = fmt.Errorf("jsingo: Module(%q): entry must be a relative path inside the filesystem", entry)
	default:
		if _, err := fs.Stat(fsys, m.entry); err != nil {
			// Overwhelmingly this is an embed pattern that does not cover the
			// entry, so the message says so rather than only reporting ENOENT.
			m.err = fmt.Errorf(
				"jsingo: Module(%q): entry not found in the embedded filesystem. "+
					"Check the go:embed pattern actually includes it, for example "+
					"//go:embed %s: %w", entry, m.entry, err)
		}
	}
	m.name = moduleName(m.entry)
	return m
}

// Name returns the module's name, used to qualify its exports.
func (m *Mod) Name() string { return m.name }

// Entry returns the entrypoint path within the module's filesystem.
func (m *Mod) Entry() string { return m.entry }

// moduleName derives a module name from an entry path: "js/article.ts" becomes
// "article".
func moduleName(entry string) string {
	base := path.Base(entry)
	for _, ext := range []string{".ts", ".tsx", ".mts", ".cts", ".js", ".mjs", ".cjs", ".jsx"} {
		if strings.HasSuffix(base, ext) {
			return strings.TrimSuffix(base, ext)
		}
	}
	return base
}

// files lists every regular file in the module, sorted.
func (m *Mod) files() ([]string, error) {
	var out []string
	err := fs.WalkDir(m.fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// A node_modules inside an embedded filesystem means the embed
			// pattern was too broad. Refuse rather than write it to disk:
			// silently materialising thousands of files would look like a
			// hang, and it is never what was intended.
			if d.Name() == "node_modules" {
				return fmt.Errorf(
					"jsingo: module %q embeds a node_modules directory at %q. "+
						"Use explicit go:embed patterns (js/*.ts js/package.json) "+
						"rather than embedding the directory", m.name, p)
			}
			return nil
		}
		out = append(out, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}

// hash fingerprints the module's contents.
//
// Used to key the extraction cache, so identical content is written once and
// changed content never reuses a stale directory. Paths are included so a
// rename alone changes the fingerprint.
func (m *Mod) hash() (string, error) {
	files, err := m.files()
	if err != nil {
		return "", err
	}

	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00", f)
		b, err := fs.ReadFile(m.fsys, f)
		if err != nil {
			return "", fmt.Errorf("jsingo: hash module %q: %w", m.name, err)
		}
		fmt.Fprintf(h, "%d\x00", len(b))
		h.Write(b)
	}
	return hex.EncodeToString(h.Sum(nil))[:32], nil
}
