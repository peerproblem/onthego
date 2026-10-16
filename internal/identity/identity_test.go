package identity

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestIdentityCreationAndReloadArePrivateAndStable(t *testing.T) {
	base := filepath.Join(t.TempDir(), ".onthego")
	t.Setenv("ONTHEGO_HOME", base)
	first, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Validate(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "identity", "keys.json")
	for path, mode := range map[string]os.FileMode{base: 0o700, filepath.Dir(file): 0o700, file: 0o600} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("insecure permissions for %s: %v, %v", path, info, err)
		}
		if err := os.Chmod(path, 0o777); err != nil {
			t.Fatal(err)
		}
	}
	second, err := LoadOrCreate()
	if err != nil || first != second {
		t.Fatalf("identity changed on reload: %v", err)
	}
	for path, mode := range map[string]os.FileMode{base: 0o700, filepath.Dir(file): 0o700, file: 0o600} {
		info, _ := os.Stat(path)
		if info.Mode().Perm() != mode {
			t.Fatalf("did not tighten permissions for %s", path)
		}
	}
}

func TestInvalidExistingIdentityIsNeverSilentlyReplaced(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", base)
	keys, err := LoadOrCreate()
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "identity", "keys.json")
	for name, mutate := range map[string]func(*Keys){
		"missing age identity":      func(k *Keys) { k.AgeIdentity = "" },
		"mismatched age recipient":  func(k *Keys) { k.AgeRecipient = "wrong" },
		"short signing key":         func(k *Keys) { k.SigningPrivate = base64.StdEncoding.EncodeToString([]byte("short")) },
		"mismatched signing public": func(k *Keys) { k.SigningPublic = base64.StdEncoding.EncodeToString(make([]byte, 32)) },
		"inconsistent private key":  func(k *Keys) { k.SigningPrivate = base64.StdEncoding.EncodeToString(make([]byte, 64)) },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := keys
			mutate(&invalid)
			data, err := json.Marshal(invalid)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(); err == nil {
				t.Fatal("accepted invalid existing identity")
			}
			after, err := os.ReadFile(file)
			if err != nil || string(after) != string(data) {
				t.Fatalf("invalid identity was changed: %v", err)
			}
		})
	}
}

func TestIdentityRejectsSymlinkedKeyFile(t *testing.T) {
	base := t.TempDir()
	t.Setenv("ONTHEGO_HOME", base)
	if _, err := LoadOrCreate(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(base, "identity", "keys.json")
	outside := filepath.Join(t.TempDir(), "keys.json")
	if err := os.Rename(file, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, file); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreate(); err == nil {
		t.Fatal("accepted symlinked identity")
	}
}

func TestConcurrentIdentityCreationReturnsOneIdentity(t *testing.T) {
	t.Setenv("ONTHEGO_HOME", t.TempDir())
	const count = 12
	var wg sync.WaitGroup
	keys := make([]Keys, count)
	errors := make([]error, count)
	start := make(chan struct{})
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			keys[i], errors[i] = LoadOrCreate()
		}()
	}
	close(start)
	wg.Wait()
	for i := range count {
		if errors[i] != nil {
			t.Fatalf("concurrent creation %d: %v", i, errors[i])
		}
		if keys[i] != keys[0] {
			t.Fatal("concurrent callers received different identities")
		}
	}
}
