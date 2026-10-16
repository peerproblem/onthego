package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/gitx"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/restore"
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
	if err == nil {
		return 0
	}
	e := otgerror.Public(err)
	switch e.Code {
	case otgerror.CodeCanceled:
		return 130
	case otgerror.CodeInternal:
		return 1
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
	c.SetFlagErrorFunc(func(_ *cobra.Command, err error) error {
		return otgerror.Wrap(otgerror.CodeInput, "invalid command arguments", err)
	})
	c.PersistentFlags().BoolVar(&a.verbose, "verbose", false, "show handoff phase progress on stderr")
	c.PersistentFlags().BoolVar(&a.json, "json", false, "write JSON output")
	c.AddCommand(a.initCommand(), a.deviceCommand(), a.remoteCommand(), a.backupsCommand(), a.undoCommand(), a.pushCommand(), a.pullCommand(), a.statusCommand(), a.inspectCommand(), a.doctorCommand(), a.snapshotCommand(), a.identityCommand(), a.configCommand(), a.peerCommand(), a.sessionsCommand())
	c.AddCommand(&cobra.Command{Use: "version", Args: cobra.NoArgs, Short: "Show build information", RunE: func(c *cobra.Command, _ []string) error {
		return a.print(c, map[string]any{"version": a.version, "commit": a.commit, "built_at": a.builtAt}, fmt.Sprintf("onthego %s (%s)", a.version, a.commit))
	}})
	typeArgumentErrors(c)
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
		if backups, backupErr := restore.Backups(context.Background(), root); backupErr == nil && len(backups) > 0 {
			return "", cfg, otgerror.New(otgerror.CodeInput, "project policy is missing; use onthego undo or git checkout -- .onthego.yaml to recover it first; onthego init creates a new project ID")
		}
		return "", cfg, otgerror.New(otgerror.CodeInput, "run onthego init in this project first")
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
		return a.print(c, map[string]any{"schema_version": 2, "project_id": cfg.ProjectID, "root": root, "config": path}, "Initialized project "+terminalValue(cfg.ProjectID)+"\nMachine settings: "+terminalValue(path)+"\nAdd a device with onthego device add.")
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
		return a.print(c, map[string]string{"path": p}, terminalValue(p))
	}})
	return c
}

func loadGitProject(ctx context.Context) (string, error) {
	root, err := config.FindRoot(".")
	if err != nil {
		return "", err
	}
	if err = gitx.Validate(ctx, root); err != nil {
		return "", err
	}
	return root, nil
}

func typeArgumentErrors(c *cobra.Command) {
	// Cobra normally flushes deprecated flag warnings through command stdout.
	// Route pflag diagnostics directly to stderr so JSON output stays parseable.
	c.Flags().SetOutput(commandErrorWriter{c})
	if validate := c.Args; validate != nil {
		c.Args = func(c *cobra.Command, args []string) error {
			if err := validate(c, args); err != nil {
				return otgerror.Wrap(otgerror.CodeInput, "invalid command arguments", err)
			}
			return nil
		}
	}
	for _, child := range c.Commands() {
		typeArgumentErrors(child)
	}
}

type commandErrorWriter struct{ command *cobra.Command }

func (w commandErrorWriter) Write(data []byte) (int, error) {
	return w.command.ErrOrStderr().Write(data)
}
