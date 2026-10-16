package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEnsureGitignorePreservesContentAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, ".gitignore")
	if err := os.WriteFile(path, []byte("node_modules/\n.env\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	updated, err := EnsureGitignore(root)
	if err != nil {
		t.Fatal(err)
	}
	if !updated {
		t.Fatal("expected .gitignore update")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, expected := range append([]string{"node_modules/"}, gitignoreEntries...) {
		if !strings.Contains(text, expected+"\n") {
			t.Fatalf("missing %q in %s", expected, text)
		}
	}
	if strings.Count("\n"+text, "\n.env\n") != 1 {
		t.Fatalf("duplicated existing entry: %s", text)
	}
	for _, shared := range []string{"AGENTS.md", ".agents/", "HANDOFF.md"} {
		if strings.Contains(text, shared) {
			t.Fatalf("shared agent context must remain versionable: %s", shared)
		}
	}
	updated, err = EnsureGitignore(root)
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("second update should be a no-op")
	}
}

func TestLegacyProjectMigrationPreservesPolicyAndRemovesCloudFields(t *testing.T) {
	root := t.TempDir()
	legacy := "schema_version: 1\nproject_id: legacy-project\nagent: codex\ninclude: [notes, ignored.dat]\nexclude: [cache]\nsummary: {kind: openai}\ntargets: {daytona: {provider: daytona}}\nworkloads: {old: {kind: agent}}\nsecrets: {old: {name: api-key}}\n"
	if err := os.WriteFile(filepath.Join(root, FileName), []byte(legacy), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(root)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SchemaVersion != SchemaVersion || cfg.ProjectID != "legacy-project" || cfg.Agent != "codex" || !reflect.DeepEqual(cfg.Include, []string{"notes", "ignored.dat"}) || !reflect.DeepEqual(cfg.Exclude, []string{"cache"}) {
		t.Fatalf("migration changed project policy: %+v", cfg)
	}
	if err := Save(root, cfg); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, FileName))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"summary:", "targets:", "workloads:", "secrets:", "api-key", "daytona"} {
		if strings.Contains(string(data), field) {
			t.Fatalf("obsolete cloud field persisted: %s", field)
		}
	}
}

func TestProjectIDsCannotEscapeDataRoot(t *testing.T) {
	t.Setenv("ONTHEGO_HOME", t.TempDir())
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\b`, "a\x00b", "/tmp", "white space", "C:drive"} {
		if _, err := ProjectDataRoot(id); err == nil {
			t.Errorf("accepted unsafe project ID %q", id)
		}
	}
	for _, id := range []string{"project", "a.1-b_2", "ba8ea17e-f314-4676-a76c-246d6c43b7f0"} {
		if _, err := ProjectDataRoot(id); err != nil {
			t.Errorf("rejected valid project ID %q: %v", id, err)
		}
	}
}

func TestDataRootUsesPersonalHomeAndExplicitOverrides(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("ONTHEGO_HOME", "")
	t.Setenv("ONTHEGO_DATA_HOME", "")
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, "xdg"))
	assertRoot := func(want string) {
		t.Helper()
		got, err := DataRoot()
		if err != nil || got != want {
			t.Fatalf("DataRoot() = %q, %v; want %q", got, err, want)
		}
	}
	assertRoot(filepath.Join(home, ".onthego"))
	legacy := filepath.Join(home, "legacy")
	t.Setenv("ONTHEGO_DATA_HOME", legacy)
	assertRoot(filepath.Join(home, ".onthego"))
	preferred := filepath.Join(home, "preferred")
	t.Setenv("ONTHEGO_HOME", preferred)
	assertRoot(preferred)
}

func TestUserConfigPrivateRoundTrip(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".onthego")
	t.Setenv("ONTHEGO_HOME", base)
	cfg, err := LoadUser()
	if err != nil || cfg.SchemaVersion != SchemaVersion || len(cfg.Devices) != 0 {
		t.Fatalf("initial config = %+v, %v", cfg, err)
	}
	cfg.Devices["desktop"] = Device{Host: "me@mac-mini", Path: "/Users/me/work/my app", Port: 2222, IdentityFile: "/Users/me/.ssh/id_ed25519"}
	cfg.Devices["local"] = Device{LocalPath: "/tmp/second checkout", Binary: "/Users/me/local tools/onthego's wrapper"}
	if err := SaveUser(cfg); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, UserFileName)
	for path, mode := range map[string]os.FileMode{base: 0o700, file: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("insecure %s permissions: %v, %v", path, info, err)
		}
	}
	if err := os.Chmod(base, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadUser()
	if err != nil || !reflect.DeepEqual(cfg, loaded) {
		t.Fatalf("loaded config = %+v, %v; want %+v", loaded, err, cfg)
	}
	for path, mode := range map[string]os.FileMode{base: 0o700, file: 0o600} {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != mode {
			t.Fatalf("did not tighten permissions for %s", path)
		}
	}
}

func TestValidateDeviceRejectsInjectionAndAmbiguousTransports(t *testing.T) {
	for _, host := range []string{"-oProxyCommand=evil", "name;evil", "user@host evil", "$(evil)", "host\n-oPort=2", "user@@host", "-host", "user@-host", "user`evil`@host"} {
		if err := ValidateDevice(Device{Host: host, Path: "/work/project"}); err == nil {
			t.Errorf("accepted unsafe host %q", host)
		}
	}
	for _, device := range []Device{
		{},
		{Host: "host", LocalPath: "/tmp/project", Path: "/project"},
		{Host: "host", Path: "../project"},
		{Host: "host", Path: "/work\nproject"},
		{Host: "host", Path: "/work", Port: -1},
		{Host: "host", Path: "/work", Port: 65536},
		{Host: "host", Path: "/work", Binary: "onthego;evil"},
		{Host: "host", Path: "/work", IdentityFile: "-evil"},
		{LocalPath: "relative"},
		{LocalPath: "/work", Port: 22},
		{LocalPath: "/work", AgeRecipient: "invalid"},
		{LocalPath: "/work", SigningPublic: "invalid"},
	} {
		if err := ValidateDevice(device); err == nil {
			t.Errorf("accepted invalid device %+v", device)
		}
	}
	for _, host := range []string{"mini", "alice@my-mini.local", "192.168.1.2", "user@[::1]", "user@2001:db8::1"} {
		if err := ValidateDevice(Device{Host: host, Path: "/work/my project", Binary: "/usr/local/bin/onthego"}); err != nil {
			t.Errorf("rejected valid host %q: %v", host, err)
		}
	}
}

func TestUserConfigRejectsSymlinksAndUnknownSecretFields(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", base)
	file := filepath.Join(base, UserFileName)
	outside := filepath.Join(t.TempDir(), "outside.yaml")
	if err := os.WriteFile(outside, []byte("schema_version: 2\ndevices: {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(); err == nil {
		t.Fatal("loaded symlinked user config")
	}
	if err := SaveUser(UserConfig{}); err == nil {
		t.Fatal("replaced symlinked user config")
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, []byte("schema_version: 2\ndevices: {}\napi_key: should-not-be-stored\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadUser(); err == nil {
		t.Fatal("accepted unsupported credential field")
	}
}

func TestDevicesAreIndependentOfProjectPaths(t *testing.T) {
	t.Setenv("ONTHEGO_HOME", t.TempDir())
	user := UserConfig{Devices: map[string]Device{"laptop": {Host: "me@laptop", Home: "/Users/me"}}}
	if err := SaveUser(user); err != nil {
		t.Fatal(err)
	}
	for id, path := range map[string]string{"app-one": "/work/one", "app-two": "/work/two"} {
		if err := SetProjectPeer(id, "laptop", path); err != nil {
			t.Fatal(err)
		}
		peers, err := LoadProjectPeers(id)
		if err != nil || peers.Peers["laptop"] != path {
			t.Fatalf("peers %s = %+v, %v", id, peers, err)
		}
		file, _ := ProjectPeersPath(id)
		info, _ := os.Stat(file)
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("peers permissions = %v", info.Mode())
		}
	}
	loaded, err := LoadUser()
	if err != nil || len(loaded.Devices) != 1 || loaded.Devices["laptop"].Path != "" {
		t.Fatalf("devices changed: %+v, %v", loaded, err)
	}
	if err := SetProjectPeer("app-one", "laptop", "../escape"); err == nil {
		t.Fatal("relative peer path accepted")
	}
}
