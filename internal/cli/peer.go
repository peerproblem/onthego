package cli

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/peerproblem/onthego/internal/agentsessions"
	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/otgerror"
	"github.com/peerproblem/onthego/internal/syncer"
	"github.com/spf13/cobra"
)

// The peer protocol runs only as an SSH/local subprocess, never as a listening server.
func (a *app) peerCommand() *cobra.Command {
	c := &cobra.Command{Use: "peer", Hidden: true, Short: "Private subprocess protocol for direct device transfer"}
	c.AddCommand(&cobra.Command{Use: "identity", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		v, err := localIdentity()
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(v)
	}})
	c.AddCommand(a.helloCommand(), a.trustCommand())
	var source, sender, recipientSnapshot string
	var recipientTips []string
	var selection agentsessions.Selection
	export := &cobra.Command{Use: "export", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(source) || sender == "" {
			return errors.New("absolute --root and --from are required")
		}
		trusted, err := trustedIdentity(sender)
		if err != nil {
			return err
		}
		if recipientSnapshot != "" {
			if err = syncer.ReconcilePending(c.Context(), source, recipientSnapshot, sender); err != nil {
				return err
			}
		}
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		if _, err = syncer.ExportWithOptions(c.Context(), source, path, trusted.AgeRecipient, a.version, syncer.ExportOptions{Peer: sender, RecipientTips: recipientTips, Selection: selection}); err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(c.OutOrStdout(), f)
		return err
	}}
	export.Flags().StringVar(&source, "root", "", "absolute project root")
	export.Flags().StringVar(&sender, "from", "", "paired receiver identity")
	export.Flags().StringVar(&recipientSnapshot, "recipient-snapshot", "", "receiver last applied snapshot for lost-ack recovery")
	export.Flags().StringSliceVar(&recipientTips, "recipient-tip", nil, "receiver Git object tip")
	addSelectionFlags(export, &selection)
	var destination, from, policy string
	var replace bool
	apply := &cobra.Command{Use: "apply", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if err := validateSessionPolicy(policy); err != nil {
			return err
		}
		if !filepath.IsAbs(destination) || from == "" {
			return errors.New("absolute --root and --from are required")
		}
		trusted, err := trustedIdentity(from)
		if err != nil {
			return err
		}
		path, cleanup, err := tempPackage()
		if err != nil {
			return err
		}
		defer cleanup()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(f, c.InOrStdin())
		closeErr := f.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		result, err := syncer.ApplyWithOptions(c.Context(), path, destination, trusted.SigningPublic, syncer.ApplyOptions{Replace: replace, Peer: from, Sessions: agentsessions.ImportOptions{ConflictPolicy: policy}})
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(result)
	}}
	apply.Flags().StringVar(&destination, "root", "", "absolute destination root")
	apply.Flags().StringVar(&from, "from", "", "paired sender identity; signing key is resolved locally")
	apply.Flags().StringVar(&policy, "sessions", "keep-both", "divergent session policy")
	apply.Flags().BoolVar(&replace, "replace", false, "preserve a backup then replace conflicting destination")
	var statusRoot string
	status := &cobra.Command{Use: "status", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(statusRoot) {
			return errors.New("absolute --root is required")
		}
		if _, err := os.Stat(statusRoot); os.IsNotExist(err) {
			return json.NewEncoder(c.OutOrStdout()).Encode(syncer.StatusInfo{Root: statusRoot})
		}
		result, err := syncer.Status(c.Context(), statusRoot)
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(result)
	}}
	status.Flags().StringVar(&statusRoot, "root", "", "absolute project root")
	var ackRoot, snapshotID, ackFrom string
	ack := &cobra.Command{Use: "ack", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if !filepath.IsAbs(ackRoot) || snapshotID == "" {
			return errors.New("absolute --root and --snapshot are required")
		}
		if _, err := trustedIdentity(ackFrom); err != nil {
			return err
		}
		if err := syncer.MarkSyncedPeer(c.Context(), ackRoot, snapshotID, ackFrom); err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"schema_version": 2, "snapshot_id": snapshotID, "acknowledged": true})
	}}
	ack.Flags().StringVar(&ackRoot, "root", "", "absolute source root")
	ack.Flags().StringVar(&ackFrom, "from", "", "paired receiver identity")
	ack.Flags().StringVar(&snapshotID, "snapshot", "", "successfully received snapshot ID")
	c.AddCommand(export, apply, status, ack)
	return c
}

func (a *app) helloCommand() *cobra.Command {
	return &cobra.Command{Use: "hello", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		home, err := os.UserHomeDir()
		if err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(helloResponse{ProtocolMajor: protocolMajor, Version: a.version, Capabilities: protocolCapabilities, Home: home})
	}}
}
func trustedIdentity(name string) (config.TrustedIdentity, error) {
	cfg, err := config.LoadUser()
	if err != nil {
		return config.TrustedIdentity{}, err
	}
	trusted, ok := cfg.Trusted[name]
	if !ok {
		return trusted, otgerror.New(otgerror.CodeTargetAuth, "sender is not paired; run device add from the sending machine")
	}
	return trusted, nil
}
func (a *app) trustCommand() *cobra.Command {
	var name string
	var pub config.TrustedIdentity
	c := &cobra.Command{Use: "trust", Args: cobra.NoArgs, RunE: func(c *cobra.Command, _ []string) error {
		if err := config.ValidateDeviceName(name); err != nil {
			return err
		}
		if name != identityName(pub.SigningPublic) {
			return errors.New("trust name must match the signing identity fingerprint")
		}
		cfg, err := config.LoadUser()
		if err != nil {
			return err
		}
		if old, ok := cfg.Trusted[name]; ok && old != pub {
			return otgerror.New(otgerror.CodeTargetAuth, "paired identity changed; remove the previous pairing explicitly")
		}
		if cfg.Trusted == nil {
			cfg.Trusted = map[string]config.TrustedIdentity{}
		}
		cfg.Trusted[name] = pub
		if err = config.SaveUser(cfg); err != nil {
			return err
		}
		return json.NewEncoder(c.OutOrStdout()).Encode(map[string]any{"trusted": name})
	}}
	c.Flags().StringVar(&name, "name", "", "stable sender identity name")
	c.Flags().StringVar(&pub.AgeRecipient, "age-recipient", "", "sender public encryption recipient")
	c.Flags().StringVar(&pub.SigningPublic, "signing-public", "", "sender public signing key")
	return c
}
