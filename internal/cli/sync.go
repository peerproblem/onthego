package cli

import (
	"fmt"

	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

// syncPlan is the direction chosen by onthego sync and the reason shown to the user.
type syncPlan struct {
	Direction string `json:"direction"`
	Reason    string `json:"reason"`
}

func (a *app) syncCommand() *cobra.Command {
	var dry bool
	var push pushOptions
	c := &cobra.Command{Use: "sync <device>", Args: cobra.ExactArgs(1), ValidArgsFunction: completeDevices, Short: "Push or pull, whichever direction brings both devices up to date", Long: `Compare this workspace with the same project on a paired device and run
push or pull in the direction that is safe. Nothing is merged: when both sides
changed since the last handoff, sync stops and tells you which explicit command
keeps which side.`, RunE: func(c *cobra.Command, args []string) error {
		if err := validateTransferOptions(push.selection, push.policy, nil, nil); err != nil {
			return err
		}
		root, cfg, err := loadProject()
		if err != nil {
			return err
		}
		d, err := device(args[0])
		if err != nil {
			return err
		}
		d, err = resolveDevicePath(args[0], d, root, cfg)
		if err != nil {
			return err
		}
		a.phase(c, "Checking peer protocol and handoff state on "+args[0])
		if _, err = negotiatePeer(c.Context(), d); err != nil {
			return err
		}
		var remote syncer.StatusInfo
		if err = peerJSON(c.Context(), d, []string{"peer", "status", "--root", devicePath(d)}, nil, &remote); err != nil {
			return err
		}
		local, err := syncer.Status(c.Context(), root)
		if err != nil {
			return err
		}
		if remote.SnapshotID != "" && remote.SnapshotID != local.SnapshotID {
			// A lost acknowledgement leaves the delivered export pending here.
			if err = syncer.ReconcilePending(c.Context(), root, remote.SnapshotID, args[0]); err != nil {
				return err
			}
			if local, err = syncer.Status(c.Context(), root); err != nil {
				return err
			}
		}
		plan, err := decideSync(args[0], local, remote)
		if err != nil {
			return err
		}
		if dry || plan.Direction == "none" {
			message := plan.Reason
			switch plan.Direction {
			case "push":
				message = "Would push to " + terminalValue(args[0]) + ": " + plan.Reason
			case "pull":
				message = "Would pull from " + terminalValue(args[0]) + ": " + plan.Reason
			}
			return a.print(c, map[string]any{"device": args[0], "direction": plan.Direction, "reason": plan.Reason, "dry_run": dry}, message)
		}
		if !a.json {
			fmt.Fprintln(c.ErrOrStderr(), "Sync: "+plan.Direction+" ("+plan.Reason+")")
		}
		if plan.Direction == "push" {
			return a.runPush(c, args, push)
		}
		return a.runPull(c, args, pullOptions{policy: push.policy, selection: push.selection})
	}}
	c.Flags().BoolVar(&dry, "dry-run", false, "show the chosen direction without transferring")
	addSessionPolicyFlags(c, &push.policy)
	addSelectionFlags(c, &push.selection)
	return c
}

// decideSync never guesses between two diverged workspaces. It only moves work
// away from a side that has not changed since the last handoff.
func decideSync(name string, local, remote syncer.StatusInfo) (syncPlan, error) {
	if remote.ProjectID == "" {
		return syncPlan{"push", "the project does not exist on " + name + " yet"}, nil
	}
	if local.ProjectID != "" && remote.ProjectID != local.ProjectID {
		return syncPlan{}, otgerror.New(otgerror.CodePrecondition, fmt.Sprintf("%s has a different project at %s; map the right directory with onthego device path %s /absolute/path", name, remote.Root, name))
	}
	both := fmt.Sprintf("both devices changed since the last handoff; keep this device's work with onthego push %s --replace, or keep %s's work with onthego pull %s --replace (a Git backup of the replaced side is kept)", name, name, name)
	if local.HasBaseline && local.SnapshotID == remote.SnapshotID {
		switch {
		case local.Changed && remote.Changed:
			return syncPlan{}, otgerror.New(otgerror.CodeGenerationConflict, both)
		case local.Changed:
			return syncPlan{"push", "this device has new work"}, nil
		case remote.Changed:
			return syncPlan{"pull", name + " has new work"}, nil
		case local.ActiveDevice != "":
			return syncPlan{"pull", "taking the workspace back from " + local.ActiveDevice}, nil
		}
		return syncPlan{"none", "Already in sync with " + name + "."}, nil
	}
	switch {
	case local.ActiveDevice != "" && !local.Changed:
		return syncPlan{"pull", "this device handed its work to " + local.ActiveDevice + " and has not changed since"}, nil
	case remote.ActiveDevice != "" && !remote.Changed:
		return syncPlan{"push", name + " handed its work off and has not changed since"}, nil
	case !local.HasBaseline && !remote.HasBaseline:
		return syncPlan{}, otgerror.New(otgerror.CodePrecondition, fmt.Sprintf("this project exists on both devices but was never handed off; choose a direction with onthego push %s or onthego pull %s (add --replace to overwrite the other side after a Git backup)", name, name))
	case local.Changed && remote.Changed:
		return syncPlan{}, otgerror.New(otgerror.CodeGenerationConflict, both)
	}
	return syncPlan{}, otgerror.New(otgerror.CodePrecondition, fmt.Sprintf("cannot tell which side is newer; choose a direction with onthego push %s or onthego pull %s", name, name))
}
