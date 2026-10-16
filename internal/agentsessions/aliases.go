package agentsessions

import (
	"bytes"
	"errors"
	"path/filepath"
	"strings"

	"github.com/jbaehova/onthego/internal/fsx"
)

// Source aliases are recorded on the source machine, since a receiver on a
// different OS cannot resolve macOS /var aliases or a source's mount paths.
// Only metadata path tokens are inspected; transcript content is opaque.
func collectAliases(data []byte, root string, aliases map[string]string) {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		parser := pathRewriter{data: line, transform: func(value string) string {
			if !filepath.IsAbs(value) {
				return value
			}
			canonical, err := fsx.Canonical(value)
			if err != nil || canonical == value || !lexicallyWithin(canonical, root) {
				return value
			}
			alias, canonicalBase := filepath.Clean(value), canonical
			// Prefer a project-root alias over one entry per file. Verify this on the
			// source before recording it, including aliases to project subdirectories.
			rel, err := filepath.Rel(root, canonical)
			if err == nil {
				candidate := alias
				if rel != "." {
					for range strings.Split(rel, string(filepath.Separator)) {
						candidate = filepath.Dir(candidate)
					}
				}
				resolved, err := fsx.Canonical(candidate)
				if err == nil && resolved == root && candidate != string(filepath.Separator) {
					alias, canonicalBase = candidate, root
				}
			}
			if alias != canonicalBase && alias != string(filepath.Separator) {
				aliases[alias] = canonicalBase
			}
			return value
		}}
		parser.value(false, false)
	}
}

func lexicallyWithin(path, root string) bool {
	return filepath.IsAbs(path) && (path == root || strings.HasPrefix(path, root+string(filepath.Separator)))
}

func validateAliases(aliases map[string]string, root string) error {
	for alias, canonical := range aliases {
		if !filepath.IsAbs(alias) || filepath.Clean(alias) != alias || alias == string(filepath.Separator) || strings.ContainsRune(alias, 0) || filepath.Clean(canonical) != canonical || strings.ContainsRune(canonical, 0) || !lexicallyWithin(canonical, root) {
			return errors.New("invalid native session source path alias")
		}
	}
	return nil
}

// Use the longest exact path-segment prefix, never a textual substring, and do
// not follow alias chains supplied in a manifest.
func aliasPath(value string, aliases map[string]string) string {
	longest, mapped := "", ""
	for alias, canonical := range aliases {
		if len(alias) > len(longest) && (value == alias || strings.HasPrefix(value, alias+string(filepath.Separator))) {
			longest, mapped = alias, canonical
		}
	}
	if longest != "" {
		return mapped + strings.TrimPrefix(value, longest)
	}
	return value
}
