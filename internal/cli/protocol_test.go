package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/peerproblem/onthego/internal/config"
	"github.com/peerproblem/onthego/internal/identity"
)

func TestPeerApplyRejectsUnpairedSenderBeforeReadingPackage(t *testing.T) {
	t.Setenv("ONTHEGO_HOME", t.TempDir())
	a := &app{}
	c := a.rootCommand()
	c.SetArgs([]string{"peer", "apply", "--root", filepath.Join(t.TempDir(), "target"), "--from", "unpaired"})
	c.SetIn(strings.NewReader("untrusted ciphertext"))
	if err := c.ExecuteContext(context.Background()); err == nil || !strings.Contains(err.Error(), "not paired") {
		t.Fatalf("unpaired apply = %v", err)
	}
	c = a.rootCommand()
	c.SetArgs([]string{"peer", "apply", "--root", filepath.Join(t.TempDir(), "target"), "--signing-public", "caller-controlled"})
	if err := c.Execute(); err == nil {
		t.Fatal("legacy caller supplied signing public key accepted")
	}
}
func TestPeerTrustPinsIdentityAndHelloDeclaresProtocol(t *testing.T) {
	t.Setenv("ONTHEGO_HOME", t.TempDir())
	keys, err := identity.LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	name := identityName(keys.SigningPublic)
	a := &app{version: "0.3.0-test"}
	c := a.rootCommand()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetArgs([]string{"peer", "trust", "--name", name, "--age-recipient", keys.AgeRecipient, "--signing-public", keys.SigningPublic})
	if err = c.Execute(); err != nil {
		t.Fatal(err)
	}
	pinned, err := trustedIdentity(name)
	if err != nil || pinned.SigningPublic != keys.SigningPublic {
		t.Fatalf("pinned = %+v, %v", pinned, err)
	}
	c = a.rootCommand()
	c.SetOut(&out)
	out.Reset()
	c.SetArgs([]string{"peer", "hello"})
	if err = c.Execute(); err != nil {
		t.Fatal(err)
	}
	var hello helloResponse
	if err = json.Unmarshal(out.Bytes(), &hello); err != nil {
		t.Fatal(err)
	}
	if hello.ProtocolMajor != 3 || hello.Home == "" || hello.Version != "0.3.0-test" || len(hello.Capabilities) == 0 {
		t.Fatalf("hello=%+v", hello)
	}
}
func TestNegotiationRejectsIncompatiblePeer(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "peer")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s\\n' '{\"protocol_major\":99,\"version\":\"future\"}'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := negotiatePeer(context.Background(), config.Device{Local: true, Binary: binary}); err == nil || !strings.Contains(err.Error(), "incompatible") {
		t.Fatalf("negotiation = %v", err)
	}
}
func TestDefaultProjectMappingFollowsHomeRelativePath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ONTHEGO_HOME", filepath.Join(home, ".onthego"))
	cfg := config.Config{ProjectID: "project"}
	d := config.Device{Host: "laptop", Home: "/Users/remote"}
	mapped, err := resolveDevicePath("laptop", d, filepath.Join(home, "work", "app"), cfg)
	if err != nil || mapped.Path != "/Users/remote/work/app" {
		t.Fatalf("mapping = %+v, %v", mapped, err)
	}
	if err = config.SetProjectPeer(cfg.ProjectID, "laptop", "/srv/custom"); err != nil {
		t.Fatal(err)
	}
	mapped, err = resolveDevicePath("laptop", d, filepath.Join(home, "work", "app"), cfg)
	if err != nil || mapped.Path != "/srv/custom" {
		t.Fatalf("override = %+v, %v", mapped, err)
	}
}
