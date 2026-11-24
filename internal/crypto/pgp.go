package crypto

import (
	"bytes"
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/clearsign"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// Digests holds common package repository hashes.
type Digests struct {
	Size   int64
	MD5    string
	SHA1   string
	SHA256 string
	SHA512 string
}

// ComputeDigests calculates MD5, SHA1, SHA256, and SHA512 in a single pass.
func ComputeDigests(r io.Reader) (Digests, error) {
	hMD5 := md5.New()
	hSHA1 := sha1.New()
	hSHA256 := sha256.New()
	hSHA512 := sha512.New()
	n, err := io.Copy(io.MultiWriter(hMD5, hSHA1, hSHA256, hSHA512), r)
	if err != nil {
		return Digests{}, err
	}
	return Digests{
		Size:   n,
		MD5:    hex.EncodeToString(hMD5.Sum(nil)),
		SHA1:   hex.EncodeToString(hSHA1.Sum(nil)),
		SHA256: hex.EncodeToString(hSHA256.Sum(nil)),
		SHA512: hex.EncodeToString(hSHA512.Sum(nil)),
	}, nil
}

// ComputeDigestsBytes is a convenience wrapper for byte slices.
func ComputeDigestsBytes(data []byte) Digests {
	d, _ := ComputeDigests(bytes.NewReader(data))
	return d
}

// Signer wraps an OpenPGP entity for pure-Go APT/RPM signing without shelling out to gpg.
type Signer struct {
	entity *openpgp.Entity
}

// GenerateEphemeralKey generates a new RSA/EdDSA OpenPGP key pair in pure Go (useful for tests & bootstrapping).
func GenerateEphemeralKey(name, comment, email string) (*Signer, error) {
	cfg := &packet.Config{
		Algorithm: packet.PubKeyAlgoEdDSA,
	}
	ent, err := openpgp.NewEntity(name, comment, email, cfg)
	if err != nil {
		return nil, fmt.Errorf("generate openpgp key: %w", err)
	}
	return &Signer{entity: ent}, nil
}

// LoadArmoredPrivateKey loads an ASCII-armored OpenPGP private key with optional passphrase.
func LoadArmoredPrivateKey(armoredKey string, passphrase string) (*Signer, error) {
	el, err := openpgp.ReadArmoredKeyRing(strings.NewReader(armoredKey))
	if err != nil {
		return nil, fmt.Errorf("read armored key ring: %w", err)
	}
	if len(el) == 0 {
		return nil, fmt.Errorf("empty openpgp key ring")
	}
	ent := el[0]
	if ent.PrivateKey != nil && ent.PrivateKey.Encrypted {
		if err := ent.PrivateKey.Decrypt([]byte(passphrase)); err != nil {
			return nil, fmt.Errorf("decrypt openpgp primary key: %w", err)
		}
	}
	for _, sub := range ent.Subkeys {
		if sub.PrivateKey != nil && sub.PrivateKey.Encrypted {
			if err := sub.PrivateKey.Decrypt([]byte(passphrase)); err != nil {
				return nil, fmt.Errorf("decrypt openpgp subkey: %w", err)
			}
		}
	}
	return &Signer{entity: ent}, nil
}

// PublicKeyArmored exports the ASCII-armored public key (for RPM-GPG-KEY-svc-plus / APT keyring).
func (s *Signer) PublicKeyArmored() ([]byte, error) {
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PublicKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := s.entity.Serialize(w); err != nil {
		_ = w.Close()
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// PublicKeyBinary exports the binary OpenPGP public key (for /usr/share/keyrings/*.gpg).
func (s *Signer) PublicKeyBinary() ([]byte, error) {
	var buf bytes.Buffer
	if err := s.entity.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// DetachedSignArmored creates an ASCII-armored detached signature (APT Release.gpg, RPM repomd.xml.asc).
func (s *Signer) DetachedSignArmored(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := openpgp.ArmoredDetachSign(&buf, s.entity, bytes.NewReader(payload), nil); err != nil {
		return nil, fmt.Errorf("detached sign: %w", err)
	}
	return buf.Bytes(), nil
}

// ClearSign creates a clearsigned document (APT InRelease, Debian .dsc/.changes).
func (s *Signer) ClearSign(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	w, err := clearsign.Encode(&buf, s.entity.PrivateKey, nil)
	if err != nil {
		return nil, fmt.Errorf("clearsign encode: %w", err)
	}
	if _, err := w.Write(payload); err != nil {
		_ = w.Close()
		return nil, fmt.Errorf("clearsign write: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, fmt.Errorf("clearsign close: %w", err)
	}
	return buf.Bytes(), nil
}
