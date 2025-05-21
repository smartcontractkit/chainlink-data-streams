package mtls

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
)

type StaticSizedPublicKey [ed25519.PublicKeySize]byte

func (p StaticSizedPublicKey) String() string {
	return fmt.Sprintf("%x", p[:])
}

// NewTransportCredentials creates a gRPC TransportCredentials from a PrivateKey and PublicKeys set.
func NewTransportCredentials(privKey ed25519.PrivateKey, pubKeys []ed25519.PublicKey) (credentials.TransportCredentials, error) {
	priv, err := ValidPrivateKeyFromEd25519(privKey)
	if err != nil {
		return nil, err
	}
	return NewTransportSigner(priv.key, pubKeys)
}

func NewTransportSigner(signer crypto.Signer, pubKeys []ed25519.PublicKey) (credentials.TransportCredentials, error) {
	pubs, err := ValidPublicKeysFromEd25519(pubKeys...)
	if err != nil {
		return nil, err
	}

	c, err := newMutualTLSConfig(signer, pubs)
	c.ClientAuth = tls.RequireAnyClientCert
	if err != nil {
		return nil, err
	}

	return credentials.NewTLS(c), nil
}

func NewTLSConfig(privKey ed25519.PrivateKey, pubKeys []ed25519.PublicKey) (*tls.Config, error) {
	priv, err := ValidPrivateKeyFromEd25519(privKey)
	if err != nil {
		return nil, err
	}

	pubs, err := ValidPublicKeysFromEd25519(pubKeys...)
	if err != nil {
		return nil, err
	}
	c, err := newMutualTLSConfig(priv.key, pubs)

	if err != nil {
		return nil, err
	}
	c.InsecureSkipVerify = true
	c.ClientAuth = tls.RequireAnyClientCert

	return c, nil
}

func NewTLSTransportSigner(signer crypto.Signer, pubKeys []ed25519.PublicKey) (*tls.Config, error) {
	pubs, err := ValidPublicKeysFromEd25519(pubKeys...)
	if err != nil {
		return nil, err
	}

	c, err := newMutualTLSConfig(signer, pubs)
	c.ClientAuth = tls.RequireAnyClientCert
	c.InsecureSkipVerify = true

	if err != nil {
		return nil, err
	}

	return c, nil
}

// newMutualTLSConfig uses the private key and public keys to construct a mutual
// TLS 1.3 config.
//
// We provide our own peer certificate verification function to check the
// certificate's public key matches our list of registered keys.
//
// Certificates are currently used similarly to GPG keys and only functionally
// as certificates to support the crypto/tls go module.
func newMutualTLSConfig(signer crypto.Signer, pubs *PublicKeys) (*tls.Config, error) {
	cert, err := newMinimalX509Cert(signer)
	if err != nil {
		return nil, err
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},

		// Since our clients use self-signed certs, we skip verification here.
		// Instead, we use VerifyPeerCertificate for our own check.
		//
		// If VerifyPeerCertificate changes to rely on standard x509 certificate
		// fields (such as, but not limited too CN, expiration date and time)
		// then it may be necessary to reconsider the use of InsecureSkipVerify.
		InsecureSkipVerify: true, //nolint:gosec

		MaxVersion: tls.VersionTLS13,
		MinVersion: tls.VersionTLS13,

		VerifyPeerCertificate: pubs.VerifyPeerCertificate(),
	}, nil
}

// Generates a minimal certificate (that wouldn't be considered valid outside of
// this networking protocol) from an Ed25519 private key.
// This also sets the organization and organizational unit for the certificate used for User Mapping
func newMinimalX509Cert(signer crypto.Signer) (tls.Certificate, error) {
	pubKey, ok := signer.Public().(ed25519.PublicKey)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("invalid public key type")
	}

	pubKeyHex := hex.EncodeToString(pubKey)

	// Generate a random serial number.
	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("failed to generate serial number: %v", err)
	}

	now := time.Now()
	// Set certificate validity (e.g., valid for 24 hours)
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			CommonName:         pubKeyHex[:32],
			Organization:       []string{"Chainlink Data Streams"},
			OrganizationalUnit: []string{pubKeyHex},
		},
		NotBefore:             now,
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}

	encodedCert, err := x509.CreateCertificate(rand.Reader, &template, &template, signer.Public(), signer)
	if err != nil {
		return tls.Certificate{}, err
	}
	cert := tls.Certificate{
		Certificate:                  [][]byte{encodedCert},
		PrivateKey:                   signer,
		SupportedSignatureAlgorithms: []tls.SignatureScheme{tls.Ed25519},
	}
	return cert, nil
}

type PrivateKey struct {
	key ed25519.PrivateKey
}

func ValidPrivateKeyFromEd25519(key ed25519.PrivateKey) (*PrivateKey, error) {
	if len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid key length: %d, expected: %d", len(key), ed25519.PrivateKeySize)
	}

	return &PrivateKey{
		key: key,
	}, nil
}

// PublicKeys wraps a slice of keys so we can update the keys dynamically.
type PublicKeys struct {
	mu   sync.RWMutex
	keys []ed25519.PublicKey
}

func ValidPublicKeysFromEd25519(keys ...ed25519.PublicKey) (*PublicKeys, error) {
	if len(keys) == 0 {
		return nil, errors.New("no public keys provided")
	}
	for _, key := range keys {
		if len(key) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("invalid key length: %d, expected: %d", len(key), ed25519.PublicKeySize)
		}
	}

	return &PublicKeys{
		keys: keys,
	}, nil
}

func (r *PublicKeys) Keys() []ed25519.PublicKey {
	r.mu.RLock()
	defer r.mu.RUnlock()

	// Return a copy to prevent race conditions
	keysCopy := make([]ed25519.PublicKey, len(r.keys))
	copy(keysCopy, r.keys)
	return keysCopy
}

// Verifies that the certificate's public key matches with one of the keys in
// our list of registered keys.
func (r *PublicKeys) VerifyPeerCertificate() func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
	return func(rawCerts [][]byte, verifiedChains [][]*x509.Certificate) error {
		if len(rawCerts) != 1 {
			return fmt.Errorf("required exactly one client certificate")
		}
		cert, err := x509.ParseCertificate(rawCerts[0])
		if err != nil {
			return err
		}
		pk, err := pubKeyFromCert(cert)
		if err != nil {
			return err
		}

		ok := r.isValidPublicKey(pk)
		if !ok {
			return fmt.Errorf("unknown public key on cert %x", pk)
		}

		return nil
	}
}

// Replace replaces the existing keys with new keys. Use this to dynamically
// update the allowable keys at runtime.
func (r *PublicKeys) Replace(pubs *PublicKeys) {
	pubs.mu.RLock()
	newKeys := make([]ed25519.PublicKey, len(pubs.keys))
	copy(newKeys, pubs.keys)
	pubs.mu.RUnlock()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.keys = newKeys
}

// isValidPublicKey checks the public key against a list of valid keys.
func (r *PublicKeys) isValidPublicKey(pub ed25519.PublicKey) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, vpub := range r.keys {
		if subtle.ConstantTimeCompare(pub, vpub) == 1 {
			return true
		}
	}

	return false
}

// PubKeyFromCert extracts the public key from the cert and returns it as a
// statically sized byte array.
func PubKeyFromCert(cert *x509.Certificate) (StaticSizedPublicKey, error) {
	pubKey, err := pubKeyFromCert(cert)
	if err != nil {
		return StaticSizedPublicKey{}, err
	}

	return ToStaticallySizedPublicKey(pubKey)
}

// pubKeyFromCert returns an ed25519 public key extracted from the certificate.
func pubKeyFromCert(cert *x509.Certificate) (ed25519.PublicKey, error) {
	if cert.PublicKeyAlgorithm != x509.Ed25519 {
		return nil, fmt.Errorf("requires an ed25519 public key")
	}

	pub, ok := cert.PublicKey.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("invalid ed25519 public key")
	}

	return pub, nil
}

// ToStaticallySizedPublicKey converts an ed25519 public key into a statically
// sized byte array.
func ToStaticallySizedPublicKey(pubKey ed25519.PublicKey) (StaticSizedPublicKey, error) {
	var result [ed25519.PublicKeySize]byte

	if ed25519.PublicKeySize != copy(result[:], pubKey) {
		return StaticSizedPublicKey{}, errors.New("copying public key failed")
	}

	return result, nil
}
