package identity

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"filippo.io/age"
	"github.com/jbaehova/onthego/internal/config"
)

type Keys struct {
	AgeIdentity    string `json:"age_identity"`
	AgeRecipient   string `json:"age_recipient"`
	SigningPrivate string `json:"signing_private"`
	SigningPublic  string `json:"signing_public"`
}

func LoadOrCreate() (Keys, error) {
	base, err := config.DataRoot()
	if err != nil {
		return Keys{}, err
	}
	if err := config.EnsurePrivateDir(base); err != nil {
		return Keys{}, err
	}
	dir := filepath.Join(base, "identity")
	if err := config.EnsurePrivateDir(dir); err != nil {
		return Keys{}, err
	}
	path := filepath.Join(dir, "keys.json")
	keys, err := loadExisting(path)
	if err == nil {
		return keys, nil
	}
	if !os.IsNotExist(err) {
		return Keys{}, err
	}
	ageID, err := age.GenerateX25519Identity()
	if err != nil {
		return Keys{}, err
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return Keys{}, err
	}
	keys = Keys{
		AgeIdentity:    ageID.String(),
		AgeRecipient:   ageID.Recipient().String(),
		SigningPrivate: base64.StdEncoding.EncodeToString(priv),
		SigningPublic:  base64.StdEncoding.EncodeToString(pub),
	}
	data, err := json.MarshalIndent(keys, "", "  ")
	if err != nil {
		return Keys{}, err
	}
	tmp, err := os.CreateTemp(dir, "keys-*.tmp")
	if err != nil {
		return Keys{}, err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return Keys{}, err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return Keys{}, err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return Keys{}, err
	}
	if err := tmp.Close(); err != nil {
		return Keys{}, err
	}
	// Link atomically without replacing another process's freshly created key.
	// A replacement could invalidate peers that already pinned that identity.
	if err := os.Link(name, path); os.IsExist(err) {
		return loadExisting(path)
	} else if err != nil {
		return Keys{}, err
	}
	return keys, nil
}

func loadExisting(path string) (Keys, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return Keys{}, err
	}
	if !info.Mode().IsRegular() {
		return Keys{}, fmt.Errorf("identity must be a regular file: %s", path)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return Keys{}, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Keys{}, err
	}
	var keys Keys
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&keys); err != nil {
		return Keys{}, fmt.Errorf("parse identity: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Keys{}, fmt.Errorf("identity must contain one JSON object")
	}
	if err := keys.Validate(); err != nil {
		return Keys{}, fmt.Errorf("invalid existing identity: %w", err)
	}
	return keys, nil
}

func (k Keys) Validate() error {
	ageID, err := age.ParseX25519Identity(k.AgeIdentity)
	if err != nil {
		return fmt.Errorf("invalid age identity: %w", err)
	}
	if ageID.Recipient().String() != k.AgeRecipient {
		return fmt.Errorf("age identity and recipient do not match")
	}
	priv, err := k.PrivateKey()
	if err != nil {
		return err
	}
	pub, err := ParsePublic(k.SigningPublic)
	if err != nil {
		return err
	}
	if !bytes.Equal(priv.Public().(ed25519.PublicKey), pub) {
		return fmt.Errorf("signing private and public keys do not match")
	}
	return nil
}

func (k Keys) Recipient() (age.Recipient, error) {
	return age.ParseX25519Recipient(k.AgeRecipient)
}

func (k Keys) Identity() (age.Identity, error) {
	return age.ParseX25519Identity(k.AgeIdentity)
}

func (k Keys) PrivateKey() (ed25519.PrivateKey, error) {
	b, err := base64.StdEncoding.DecodeString(k.SigningPrivate)
	if err != nil || len(b) != ed25519.PrivateKeySize {
		return nil, errorsJoin("invalid signing private key", err)
	}
	if !bytes.Equal(b, ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])) {
		return nil, fmt.Errorf("invalid signing private key: inconsistent public key")
	}
	return ed25519.PrivateKey(b), nil
}

func ParsePublic(encoded string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errorsJoin("invalid signing public key", err)
	}
	return ed25519.PublicKey(b), nil
}

func errorsJoin(message string, err error) error {
	if err == nil {
		return fmt.Errorf("%s", message)
	}
	return fmt.Errorf("%s: %w", message, err)
}
