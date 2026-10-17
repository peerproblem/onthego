package cli

import (
	"testing"

	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/syncer"
)

func TestDecideSync(t *testing.T) {
	shared := func(changed bool, active string) syncer.StatusInfo {
		return syncer.StatusInfo{ProjectID: "p", SnapshotID: "s1", HasBaseline: true, Changed: changed, ActiveDevice: active}
	}
	for _, tc := range []struct {
		name          string
		local, remote syncer.StatusInfo
		want, code    string
	}{
		{"missing remote", shared(false, ""), syncer.StatusInfo{}, "push", ""},
		{"local work", shared(true, ""), shared(false, ""), "push", ""},
		{"remote work", shared(false, ""), shared(true, ""), "pull", ""},
		{"take back", shared(false, "laptop"), shared(false, ""), "pull", ""},
		{"in sync", shared(false, ""), shared(false, "desktop"), "none", ""},
		{"diverged", shared(true, ""), shared(true, ""), "", otgerror.CodeGenerationConflict},
		{"handed elsewhere", shared(false, "vps"), syncer.StatusInfo{ProjectID: "p", SnapshotID: "s2", HasBaseline: true, Changed: true}, "pull", ""},
		{"remote handed off", syncer.StatusInfo{ProjectID: "p", SnapshotID: "s2", HasBaseline: true, Changed: true}, syncer.StatusInfo{ProjectID: "p", SnapshotID: "s1", HasBaseline: true, ActiveDevice: "x"}, "push", ""},
		{"never handed off", syncer.StatusInfo{ProjectID: "p"}, syncer.StatusInfo{ProjectID: "p"}, "", otgerror.CodePrecondition},
		{"other project", shared(false, ""), syncer.StatusInfo{ProjectID: "q"}, "", otgerror.CodePrecondition},
		{"unrelated histories", syncer.StatusInfo{ProjectID: "p", SnapshotID: "a", HasBaseline: true}, syncer.StatusInfo{ProjectID: "p", SnapshotID: "b", HasBaseline: true}, "", otgerror.CodePrecondition},
	} {
		t.Run(tc.name, func(t *testing.T) {
			plan, err := decideSync("laptop", tc.local, tc.remote)
			if tc.code != "" {
				if err == nil || otgerror.Public(err).Code != tc.code {
					t.Fatalf("plan=%+v err=%v, want %s", plan, err, tc.code)
				}
				return
			}
			if err != nil || plan.Direction != tc.want {
				t.Fatalf("plan=%+v err=%v, want %s", plan, err, tc.want)
			}
		})
	}
}
