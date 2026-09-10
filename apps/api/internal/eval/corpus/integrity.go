package corpus

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// VerifyCorpus re-hashes every manifest PDF and compares it to the manifest
// sha256, requires each case directory to hold its metadata.yaml and
// expected.json sidecars, and fails on any file under the fixtures root
// that is not accounted for (manifest.yaml, a manifest-listed PDF, or a
// required sidecar). It is deterministic (manifest order; sorted extras)
// and performs no network I/O.
func VerifyCorpus(fixturesRoot string) error {
	m, err := LoadManifest(fixturesRoot)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range m.Cases {
		content, err := os.ReadFile(c.PDFPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("corpus: case %s: missing fixture %s: %v", c.ID, c.PDFPath, err))
			continue
		}
		sum := sha256.Sum256(content)
		if actual := hex.EncodeToString(sum[:]); actual != strings.ToLower(c.SHA256) {
			errs = append(errs, fmt.Errorf("corpus: case %s: sha256 mismatch (manifest %s != file %s)", c.ID, c.SHA256, actual))
		}
		for _, sidecar := range []string{metadataFileName, expectedFileName} {
			if _, err := os.Stat(filepath.Join(c.Dir(), sidecar)); err != nil {
				errs = append(errs, fmt.Errorf("corpus: case %s: missing sidecar %s: %v", c.ID, sidecar, err))
			}
		}
	}
	allowed := map[string]bool{manifestFileName: true}
	for _, c := range m.Cases {
		rel, err := filepath.Rel(fixturesRoot, c.PDFPath)
		if err != nil {
			errs = append(errs, fmt.Errorf("corpus: case %s: resolve fixture path: %v", c.ID, err))
			continue
		}
		dir := filepath.Dir(rel)
		allowed[filepath.ToSlash(rel)] = true
		allowed[filepath.ToSlash(filepath.Join(dir, metadataFileName))] = true
		allowed[filepath.ToSlash(filepath.Join(dir, expectedFileName))] = true
	}
	var extras []string
	if err := filepath.WalkDir(fixturesRoot, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(fixturesRoot, p)
		if err != nil {
			return err
		}
		if slashed := filepath.ToSlash(rel); !allowed[slashed] {
			extras = append(extras, slashed)
		}
		return nil
	}); err != nil {
		errs = append(errs, fmt.Errorf("corpus: sweep fixtures root: %w", err))
	}
	sort.Strings(extras)
	for _, e := range extras {
		errs = append(errs, fmt.Errorf("corpus: extra file not listed in manifest: %s", e))
	}
	return errors.Join(errs...)
}
