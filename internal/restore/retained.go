package restore

import (
	"context"
	"fmt"
	"sort"

	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/packageotg"
)

// Retained paths cease to be managed without deleting the recipient's copy.
// Git-tracked paths keep Git's deletion semantics even if listed as retained.
func retainUnmanagedPaths(ctx context.Context, root string, retained []string, desired map[string]gitx.TreeEntry, old map[string]bool) ([]string, error) {
	if len(retained) > packageotg.MaxFiles {
		return nil, packageInvalid("too many retained paths", nil)
	}
	seen := map[string]bool{}
	for _, path := range retained {
		if _, err := packageotg.ProjectPath(path); err != nil {
			return nil, packageInvalid("invalid retained path", err)
		}
		if seen[path] {
			return nil, packageInvalid("duplicate retained path", nil)
		}
		if _, ok := desired[path]; ok {
			return nil, packageInvalid("retained path is also desired", nil)
		}
		seen[path] = true
	}
	if len(retained) == 0 {
		return nil, nil
	}
	tracked, err := managedBefore(ctx, root, nil)
	if err != nil {
		return nil, err
	}
	key, err := pathKeyer(root)
	if err != nil {
		return nil, err
	}
	trackedKeys := map[string]bool{}
	for path := range tracked {
		trackedKeys[key(path)] = true
	}
	desiredKeys := map[string]string{}
	for path := range desired {
		desiredKeys[key(path)] = path
	}
	retainedKeys := map[string]bool{}
	var warnings []string
	for _, path := range retained {
		if alias, exists := desiredKeys[key(path)]; exists {
			return nil, packageInvalid(fmt.Sprintf("retained path %q collides with desired path %q on this filesystem", path, alias), nil)
		}
		if trackedKeys[key(path)] {
			continue
		}
		retainedKeys[key(path)] = true
		warnings = append(warnings, "No longer managed; left in place: "+path)
	}
	for path := range old {
		if retainedKeys[key(path)] {
			delete(old, path)
		}
	}
	sort.Strings(warnings)
	return warnings, nil
}

func packageInvalid(message string, cause error) error {
	return &otgerror.Error{Code: otgerror.CodePackageInvalid, Message: message, Cause: cause, Stage: "restore"}
}
