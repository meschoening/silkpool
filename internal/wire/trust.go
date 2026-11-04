package wire

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/meschoening/silkpool/internal/node"
)

// pinPrefix starts every pin: the hash it's made with.
const pinPrefix = "sha256:"

// PinOf returns the pin of cert's key, as an enrollment code carries it
// and node.Pin.Check compares it: "sha256:" and the SHA-256 of the key's
// SubjectPublicKeyInfo in lowercase hex. It pins the key, not the
// certificate, so a certificate the Hub renews with the same key still
// matches.
func PinOf(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return pinPrefix + hex.EncodeToString(sum[:])
}

// TLS returns the TLS settings an agent reaches its Hub with. With a
// pin, the Hub must present the pinned key, whatever its certificate
// says, and a Hub that doesn't is refused with an error wrapping
// node.ErrHubIdentityChanged. Without one, the Hub's certificate must
// verify through roots, or the system's trusted roots if roots is nil.
// An unverified Hub is never accepted.
func TLS(pin node.Pin, roots *x509.CertPool) *tls.Config {
	if pin == "" {
		return &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS12,
		// The pin replaces the chain and the name: VerifyConnection checks
		// the key itself, so nothing is accepted unverified.
		InsecureSkipVerify: true,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return fmt.Errorf("the Hub presented no certificate: %w", node.ErrHubIdentityChanged)
			}
			return pin.Check(PinOf(cs.PeerCertificates[0]))
		},
	}
}

// PublicKey returns a node key's public half as the Hub keeps it: the
// key in standard base64.
func PublicKey(key ed25519.PublicKey) string { return base64.StdEncoding.EncodeToString(key) }

// NonceSize is the size of a Challenge's nonce.
const NonceSize = 32

// NewNonce returns a fresh nonce for a Challenge.
func NewNonce() []byte {
	b := make([]byte, NonceSize)
	_, _ = rand.Read(b)
	return b
}

// Sign signs a Challenge's nonce for node's connection to hub, with the
// node key. The Hub ID in the signature keeps another Hub from passing
// it on.
func Sign(key ed25519.PrivateKey, nonce []byte, nodeID, hubID string) []byte {
	return ed25519.Sign(key, signed(nonce, nodeID, hubID))
}

// Verify reports whether sig is node's signature of nonce for a
// connection to hub, made with the node key whose public half is public,
// as PublicKey gives it.
func Verify(public string, nonce []byte, nodeID, hubID string, sig []byte) bool {
	key, err := base64.StdEncoding.DecodeString(public)
	if err != nil || len(key) != ed25519.PublicKeySize || len(nonce) != NonceSize {
		return false
	}
	return ed25519.Verify(ed25519.PublicKey(key), signed(nonce, nodeID, hubID), sig)
}

// signed returns what Sign signs: a fixed prefix, the nonce, and the
// node's and the Hub's IDs, each after its length.
func signed(nonce []byte, nodeID, hubID string) []byte {
	b := []byte("silkpool connection\x00")
	b = append(b, nonce...)
	for _, s := range []string{nodeID, hubID} {
		b = binary.AppendUvarint(b, uint64(len(s)))
		b = append(b, s...)
	}
	return b
}

// pinBytes returns the hash a pin holds.
func pinBytes(pin node.Pin) ([]byte, error) {
	h, ok := strings.CutPrefix(string(pin), pinPrefix)
	if !ok {
		return nil, errors.New("the pin isn't a sha256: pin")
	}
	b, err := hex.DecodeString(h)
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("the pin isn't a SHA-256 hash")
	}
	return b, nil
}
