package mtls

import (
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/rand"
)

func Test_NewTransportCredentials(t *testing.T) {
	creds, err := NewTransportCredentials(nil, nil)
	require.Error(t, err)
	assert.Nil(t, creds)

	spub, spriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	creds, err = NewTransportCredentials(spriv, []ed25519.PublicKey{spub})
	require.NoError(t, err)
	assert.Equal(t, "tls", creds.Info().SecurityProtocol)
}

func Test_NewClientTLSConfig(t *testing.T) {
	_, ed25519cpriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	spub, ed25519spriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	cpriv, err := ValidPrivateKeyFromEd25519(ed25519cpriv)
	require.NoError(t, err)

	spriv, err := ValidPrivateKeyFromEd25519(ed25519spriv)
	require.NoError(t, err)

	spubs, err := ValidPublicKeysFromEd25519(spub)
	require.NoError(t, err)

	tlsCfg, err := newMutualTLSConfig(cpriv.key, spubs)
	require.NoError(t, err)
	require.Len(t, tlsCfg.Certificates, 1)

	assert.True(t, tlsCfg.InsecureSkipVerify)
	assert.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MinVersion)
	assert.Equal(t, uint16(tls.VersionTLS13), tlsCfg.MaxVersion)

	// Test a valid server certificate
	scert, err := newMinimalX509Cert(spriv.key)
	require.NoError(t, err)

	err = tlsCfg.VerifyPeerCertificate(scert.Certificate, [][]*x509.Certificate{})
	require.NoError(t, err)

	// Test an invalid client certificate
	_, ed25519invspriv, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	invspriv, err := ValidPrivateKeyFromEd25519(ed25519invspriv)
	require.NoError(t, err)

	invscert, err := newMinimalX509Cert(invspriv.key)
	require.NoError(t, err)

	err = tlsCfg.VerifyPeerCertificate(invscert.Certificate, [][]*x509.Certificate{})
	require.Error(t, err)
}

func Test_PubKeyFromCert(t *testing.T) {
	randReader := rand.New(rand.NewSource(42)) //nolint:gosec

	pub, priv, err := ed25519.GenerateKey(randReader)
	require.NoError(t, err)

	template := x509.Certificate{SerialNumber: big.NewInt(0)}
	encodedCert, err := x509.CreateCertificate(randReader, &template, &template, priv.Public(), priv)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(encodedCert)
	require.NoError(t, err)

	actual, err := PubKeyFromCert(cert)
	require.NoError(t, err)

	assert.ElementsMatch(t, pub, actual)
}

func Test_PubKeyFromCert_MustBeEd25519KeyError(t *testing.T) {
	randReader := rand.New(rand.NewSource(42)) //nolint:gosec

	priv, err := rsa.GenerateKey(randReader, 2048)
	require.NoError(t, err)

	template := x509.Certificate{SerialNumber: big.NewInt(0)}
	encodedCert, err := x509.CreateCertificate(randReader, &template, &template, priv.Public(), priv)
	require.NoError(t, err)

	cert, err := x509.ParseCertificate(encodedCert)
	require.NoError(t, err)

	_, err = PubKeyFromCert(cert)
	require.EqualError(t, err, "requires an ed25519 public key")
}

func Test_IsValidPublicKey(t *testing.T) {
	t.Run("pub_key_included", func(t *testing.T) {
		cpub, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		pk, err := ValidPublicKeysFromEd25519(cpub)
		require.NoError(t, err)

		require.True(t, pk.isValidPublicKey(cpub))
	})

	t.Run("pub_key_not_included", func(t *testing.T) {
		cpub, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		cpub2, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		pk, err := ValidPublicKeysFromEd25519(cpub)
		require.NoError(t, err)

		// Test
		require.False(t, pk.isValidPublicKey(cpub2))
	})
}

func Test_NewPublicKeys(t *testing.T) {
	t.Run("key_length_32", func(t *testing.T) {
		cpub, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		_, err = ValidPublicKeysFromEd25519(cpub)
		require.NoError(t, err)
	})

	t.Run("key_length_not_32", func(t *testing.T) {
		shortKey := make([]byte, ed25519.PublicKeySize-1)

		_, err := ValidPublicKeysFromEd25519(shortKey)
		require.Error(t, err)

		longKey := make([]byte, ed25519.PublicKeySize+1)

		_, err = ValidPublicKeysFromEd25519(longKey)
		require.Error(t, err)
	})
}

func Test_NewTLSConfig(t *testing.T) {
	t.Run("nil_arguments", func(t *testing.T) {
		cfg, err := NewTLSConfig(nil, nil)
		require.Error(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("nil_private_key", func(t *testing.T) {
		pub, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		cfg, err := NewTLSConfig(nil, []ed25519.PublicKey{pub})
		require.Error(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("nil_public_keys", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		cfg, err := NewTLSConfig(priv, nil)
		require.Error(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("empty_public_keys", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		cfg, err := NewTLSConfig(priv, []ed25519.PublicKey{})
		require.Error(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("valid_single_key", func(t *testing.T) {
		pub, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		cfg, err := NewTLSConfig(priv, []ed25519.PublicKey{pub})
		require.NoError(t, err)
		assert.NotNil(t, cfg)
		assert.Equal(t, uint16(tls.VersionTLS13), cfg.MinVersion)
		assert.Equal(t, uint16(tls.VersionTLS13), cfg.MaxVersion)
		assert.True(t, cfg.InsecureSkipVerify)
		assert.Len(t, cfg.Certificates, 1)
	})

	t.Run("valid_multiple_keys", func(t *testing.T) {
		pub1, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		pub2, _, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		_, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		cfg, err := NewTLSConfig(priv, []ed25519.PublicKey{pub1, pub2})
		require.NoError(t, err)
		assert.NotNil(t, cfg)
		assert.Equal(t, uint16(tls.VersionTLS13), cfg.MinVersion)
		assert.Equal(t, uint16(tls.VersionTLS13), cfg.MaxVersion)
		assert.True(t, cfg.InsecureSkipVerify)
		assert.Len(t, cfg.Certificates, 1)
	})

	t.Run("invalid_public_key_length", func(t *testing.T) {
		_, priv, err := ed25519.GenerateKey(nil)
		require.NoError(t, err)

		// Create an invalid public key with wrong length
		invalidPub := make([]byte, ed25519.PublicKeySize+1)
		cfg, err := NewTLSConfig(priv, []ed25519.PublicKey{invalidPub})
		require.Error(t, err)
		assert.Nil(t, cfg)
	})
}

func Test_PublicKeys_Keys(t *testing.T) {
	// Create keys
	pub1, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	pub2, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	// Create PublicKeys with both keys
	pks, err := ValidPublicKeysFromEd25519(pub1, pub2)
	require.NoError(t, err)

	// Get a copy of the keys
	keysCopy := pks.Keys()
	require.Equal(t, 2, len(keysCopy))

	// Verify the keys match
	assert.ElementsMatch(t, []ed25519.PublicKey{pub1, pub2}, keysCopy)

	// Check original is unaffected
	keysAfter := pks.Keys()
	require.Equal(t, 2, len(keysAfter))
}

func Test_PublicKeys_Replace(t *testing.T) {
	// Create original keys
	pub1, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	pks1, err := ValidPublicKeysFromEd25519(pub1)
	require.NoError(t, err)

	// Create replacement keys
	pub2, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	pks2, err := ValidPublicKeysFromEd25519(pub2)
	require.NoError(t, err)

	// Replace keys
	pks1.Replace(pks2)

	// Verify the replacement worked
	assert.False(t, pks1.isValidPublicKey(pub1))
	assert.True(t, pks1.isValidPublicKey(pub2))

	// Modify the source after replace (shouldn't affect replaced keys)
	pub3, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	pks2.Replace(&PublicKeys{keys: []ed25519.PublicKey{pub3}})

	// Original replacement should be unaffected
	assert.True(t, pks1.isValidPublicKey(pub2))
	assert.False(t, pks1.isValidPublicKey(pub3))
}

func Test_PublicKeys_Concurrency(t *testing.T) {
	pub1, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	pub2, _, err := ed25519.GenerateKey(nil)
	require.NoError(t, err)

	pks, err := ValidPublicKeysFromEd25519(pub1)
	require.NoError(t, err)

	// Simulate concurrent reads and writes
	var wg sync.WaitGroup
	start := make(chan struct{})

	// Add multiple concurrent readers
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 10; j++ {
				_ = pks.isValidPublicKey(pub1)
				_ = pks.Keys()
				time.Sleep(time.Millisecond)
			}
		}()
	}

	// Add concurrent writers
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start

			// Alternate between pub1 and pub2
			for j := 0; j < 5; j++ {
				if (idx+j)%2 == 0 {
					pks.Replace(&PublicKeys{keys: []ed25519.PublicKey{pub1}})
				} else {
					pks.Replace(&PublicKeys{keys: []ed25519.PublicKey{pub2}})
				}
				time.Sleep(time.Millisecond * 2)
			}
		}(i)
	}

	// Start all goroutines
	close(start)
	wg.Wait()

	// No assertion needed - if there are no race conditions, the test passes
}
