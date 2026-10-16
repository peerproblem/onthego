package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/spf13/cobra"
)

type app struct {
	version, commit, builtAt string
	json                     bool
	verbose                  bool
}
type publicIdentity struct {
	SchemaVersion int    `json:"schema_version"`
	AgeRecipient  string `json:"age_recipient"`
	SigningPublic string `json:"signing_public"`
}

// Execute deliberately never reads a project .env. Each machine owns its configuration.
func Execute(version, commit, builtAt string) error {
	return ExecuteContext(context.Background(), version, commit, builtAt)
}

func ExecuteContext(ctx context.Context, version, commit, builtAt string) error {
	a := &app{version: version, commit: commit, builtAt: builtAt}
	root := a.rootCommand()
	root.SetOut(os.Stdout)
	root.SetErr(os.Stderr)
	root.SetIn(os.Stdin)
	return root.ExecuteContext(ctx)
}
func ExitCode(err error) int {
	var e *otgerror.Error
	if !errors.As(err, &e) {
		return 1
	}
	switch e.Code {
	case otgerror.CodeInput:
		return 2
	case otgerror.CodePrecondition, otgerror.CodeUnsupportedGit, otgerror.CodeSecretPolicy, otgerror.CodeSessionFormat:
		return 3
	case otgerror.CodeTargetAuth, otgerror.CodeTargetUnavailable:
		return 4
	case otgerror.CodePackageInvalid, otgerror.CodeRestoreFailed:
		return 5
	case otgerror.CodeGenerationConflict, otgerror.CodeSourceChanged:
		return 6
	}
	return 1
}
func (a *app) rootCommand() *cobra.Command {
	c := &cobra.Command{Use: "onthego", Short: "Hand off workspaces and agent sessions between your own machines", SilenceUsage: true, SilenceErrors: true}
	c.CompletionOptions.DisableDefaultCmd = true
	c.PersistentFlags().BoolVar(&a.verbose, "verbose", false, "show handoff phase progress on stderr")
	c.PersistentFlags().BoolVar(&a.json, "json", false, "write JSON output")
	c.AddCommand(a.initCommand(), a.deviceCommand(), a.remoteCommand(), a.backupsCommand(), a.undoCommand(), a.pushCommand(), a.pullCommand(), a.statusCommand(), a.inspectCommand(), a.doctorCommand(), a.snapshotCommand(), a.identityCommand(), a.configCommand(), a.peerCommand())
	c.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, Short: "Show build information", RunE: func(c *cobra.Command, _ []string) error {
		return a.print(c, map[string]any{"version": a.version, "commit": a.commit, "built_at": a.builtAt}, fmt.Sprintf("onthego %s (%s)", a.version, a.commit))
	}})
	return c
}
func (a *app) print(c *cobra.Command, v any, message string) error {
	if a.json {
		return json.NewEncoder(c.OutOrStdout()).Encode(v)
	}
	_, err := fmt.Fprintln(c.OutOrStdout(), message)
	return err
}
func loadProject() (string, config.Config, error) {
	root, err := config.FindRoot(".")
	if err != nil {
		return "", config.Config{}, err
	}
	cfg, err := config.Load(root)
	if os.IsNotExist(err) {
		return "", cfg, errors.New("run onthego init in this project first")
	}
	return root, cfg, err
}
func (a *app) initCommand() *cobra.Command {
	return &cobra.Command{Use: "init", Args: cobra.NoArgs, Short: "Initialize this Git project and private machine settings", RunE: func(c *cobra.Command, _ []string) error {
		root, err := config.FindRoot(".")
		if err != nil {
			return err
		}
		if err = gitx.Validate(c.Context(), root); err != nil {
			return err
		}
		cfg, err := config.Load(root)
		if os.IsNotExist(err) {
			cfg = config.Default()
		} else if err != nil {
			return err
		}
		if err = config.Save(root, cfg); err != nil {
			return err
		}
		if _, err = config.EnsureGitignore(root); err != nil {
			return err
		}
		if _, err = identity.LoadOrCreate(); err != nil {
			return err
		}
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if err = config.SaveUser(u); err != nil {
			return err
		}
		path, err := config.UserConfigPath()
		if err != nil {
			return err
		}
		return a.print(c, map[string]any{"schema_version": 2, "project_id": cfg.ProjectID, "root": root, "config": path}, "Initialized project "+cfg.ProjectID+"\nMachine settings: "+path+"\nAdd a device with onthego device add.")
	}}
}
func localIdentity() (publicIdentity, error) {
	k, err := identity.LoadOrCreate()
	return publicIdentity{2, k.AgeRecipient, k.SigningPublic}, err
}
func (a *app) identityCommand() *cobra.Command {
	return &cobra.Command{Use: "identity", Args: cobra.NoArgs, Short: "Show this machine's public encryption and signing keys", RunE: func(c *cobra.Command, _ []string) error {
		v, err := localIdentity()
		if err != nil {
			return err
		}
		return a.print(c, v, "Encryption recipient: "+v.AgeRecipient+"\nSigning public key: "+v.SigningPublic)
	}}
}
func (a *app) configCommand() *cobra.Command {
	c := &cobra.Command{Use: "config", Short: "Locate private machine configuration"}
	c.AddCommand(&cobra.Command{Use: "path", Args: cobra.NoArgs, Short: "Show ~/.onthego/config.yaml location", RunE: func(c *cobra.Command, _ []string) error {
		p, err := config.UserConfigPath()
		if err != nil {
			return err
		}
		return a.print(c, map[string]string{"path": p}, p)
	}})
	return c
}
