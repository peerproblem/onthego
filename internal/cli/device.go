package cli

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"filippo.io/age"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/identity"
	"github.com/peerproblem/onthego/internal/peertransport"
	"github.com/spf13/cobra"
)

func (a *app) deviceCommand() *cobra.Command {
	c := &cobra.Command{Use: "device", Short: "Manage direct connections in private machine settings"}
	var d config.Device
	add := &cobra.Command{Use: "add <name>", Args: cobra.ExactArgs(1), Short: "Pair a device identity over trusted SSH or a local transport", RunE: func(c *cobra.Command, args []string) error {
		if err := config.ValidateDeviceName(args[0]); err != nil {
			return err
		}
		if d.LocalPath != "" {
			d.Local = true
		}
		if err := config.ValidateDevice(d); err != nil {
			return err
		}
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if _, ok := u.Devices[args[0]]; ok {
			return errors.New("device already exists; remove it before pairing again")
		}
		if d.Host != "" && !c.Flags().Changed("binary") {
			d.Binary, err = peertransport.DiscoverBinary(c.Context(), endpoint(d))
			if err != nil {
				return err
			}
		}
		hello, err := negotiatePeer(c.Context(), d)
		if err != nil {
			return err
		}
		d.Home = hello.Home
		var pub publicIdentity
		if err = peerJSON(c.Context(), d, []string{"peer", "identity"}, nil, &pub); err != nil {
			return err
		}
		if pub.SchemaVersion != 2 {
			return errors.New("unsupported peer protocol; install the same onthego version on both machines")
		}
		if _, err = age.ParseX25519Recipient(pub.AgeRecipient); err != nil {
			return err
		}
		if _, err = identity.ParsePublic(pub.SigningPublic); err != nil {
			return err
		}
		d.AgeRecipient = pub.AgeRecipient
		d.SigningPublic = pub.SigningPublic
		local, err := localIdentity()
		if err != nil {
			return err
		}
		var trust map[string]any
		if err = peerJSON(c.Context(), d, []string{"peer", "trust", "--name", identityName(local.SigningPublic), "--age-recipient", local.AgeRecipient, "--signing-public", local.SigningPublic}, nil, &trust); err != nil {
			return err
		}
		target := devicePath(d)
		if target != "" {
			_, cfg, err := loadProject()
			if err != nil {
				return fmt.Errorf("path mapping requires an initialized project: %w", err)
			}
			if err = config.SetProjectPeer(cfg.ProjectID, args[0], target); err != nil {
				return err
			}
		}
		d.Path = ""
		d.LocalPath = ""
		u.Devices[args[0]] = d
		if u.Trusted == nil {
			u.Trusted = map[string]config.TrustedIdentity{}
		}
		u.Trusted[identityName(pub.SigningPublic)] = config.TrustedIdentity{AgeRecipient: pub.AgeRecipient, SigningPublic: pub.SigningPublic}
		if err = config.SaveUser(u); err != nil {
			return err
		}
		return a.print(c, map[string]any{"name": args[0], "device": d}, "Paired "+args[0]+" (mutual identity trust established)")
	}}
	add.Flags().BoolVar(&d.Local, "local", false, "pair a local executable independently of a project path")
	add.Flags().StringVar(&d.Host, "host", "", "SSH host or user@host from ~/.ssh/config")
	add.Flags().StringVar(&d.Path, "path", "", "absolute project path on the SSH peer")
	add.Flags().StringVar(&d.LocalPath, "local-path", "", "absolute destination path on this machine (no SSH)")
	add.Flags().StringVar(&d.IdentityFile, "identity-file", "", "SSH private key path; key contents stay in your SSH configuration")
	add.Flags().StringVar(&d.Binary, "binary", "onthego", "onthego executable on the peer (absolute path recommended for SSH)")
	add.Flags().IntVar(&d.Port, "port", 0, "SSH port; default uses SSH configuration")
	c.AddCommand(add, &cobra.Command{Use: "list", Args: cobra.NoArgs, Short: "List paired devices", RunE: func(c *cobra.Command, _ []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(u.Devices))
		for n := range u.Devices {
			names = append(names, n)
		}
		sort.Strings(names)
		var b strings.Builder
		for _, n := range names {
			d := u.Devices[n]
			fmt.Fprintf(&b, "%s\t%s\t%s\n", n, d.Host, d.Binary)
		}
		if len(names) == 0 {
			b.WriteString("No devices configured. Use onthego device add.")
		}
		return a.print(c, u.Devices, strings.TrimSpace(b.String()))
	}}, &cobra.Command{Use: "remove <name>", Args: cobra.ExactArgs(1), Short: "Remove a local pairing without modifying the peer", RunE: func(c *cobra.Command, args []string) error {
		u, err := config.LoadUser()
		if err != nil {
			return err
		}
		if _, ok := u.Devices[args[0]]; !ok {
			return fmt.Errorf("unknown device %q", args[0])
		}
		removed := u.Devices[args[0]]
		delete(u.Devices, args[0])
		stillUsed := false
		for _, other := range u.Devices {
			if other.SigningPublic == removed.SigningPublic {
				stillUsed = true
			}
		}
		if !stillUsed {
			delete(u.Trusted, identityName(removed.SigningPublic))
		}
		if err = config.SaveUser(u); err != nil {
			return err
		}
		return a.print(c, map[string]string{"removed": args[0]}, "Removed "+args[0])
	}})
	return c
}

func (a *app) remoteCommand() *cobra.Command {
	c := &cobra.Command{Use: "remote", Short: "Map a project to a paired device directory"}
	c.AddCommand(&cobra.Command{Use: "set <device> <path>", Args: cobra.ExactArgs(2), RunE: func(c *cobra.Command, args []string) error {
		_, cfg, err := loadProject()
		if err != nil {
			return err
		}
		if _, err = device(args[0]); err != nil {
			return err
		}
		if !filepath.IsAbs(args[1]) {
			return errors.New("remote path must be absolute")
		}
		if err = config.SetProjectPeer(cfg.ProjectID, args[0], args[1]); err != nil {
			return err
		}
		return a.print(c, map[string]string{"device": args[0], "path": args[1]}, "Mapped "+args[0]+" → "+args[1])
	}})
	return c
}
