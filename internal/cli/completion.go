package cli

import (
	"sort"
	"strings"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/spf13/cobra"
)

// completeDevices offers paired device names as the first argument.
func completeDevices(_ *cobra.Command, args []string, prefix string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	u, err := config.LoadUser()
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(u.Devices))
	for name := range u.Devices {
		if strings.HasPrefix(name, prefix) {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names, cobra.ShellCompDirectiveNoFileComp
}

func (a *app) completionCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completion <bash|zsh|fish>",
		Short:     "Print a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish"},
		Long: `Print a completion script that also completes paired device names.

  zsh:   onthego completion zsh > "${fpath[1]}/_onthego"
  bash:  onthego completion bash > ~/.local/share/bash-completion/completions/onthego
  fish:  onthego completion fish > ~/.config/fish/completions/onthego.fish`,
		RunE: func(c *cobra.Command, args []string) error {
			root := c.Root()
			out := c.OutOrStdout()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(out, true)
			case "zsh":
				return root.GenZshCompletion(out)
			case "fish":
				return root.GenFishCompletion(out, true)
			}
			return errUnsupportedShell
		},
	}
}

var errUnsupportedShell = otgerror.New(otgerror.CodeInput, "shell must be bash, zsh, or fish")
