package nats

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services/servicetest"
	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
	"github.com/stretchr/testify/require"
)

func TestClient_Connect(t *testing.T) {
	// Generate server and client keypairs
	serverPub, serverPriv, _ := ed25519.GenerateKey(rand.Reader)
	clientPub, clientPriv, _ := ed25519.GenerateKey(nil)
	clientPubHex := hex.EncodeToString(clientPub)

	serverTLSConfig, err := mtls.NewTLSConfig(serverPriv, []ed25519.PublicKey{clientPub})
	require.NoError(t, err)

	opts := &server.Options{
		Host:              "0.0.0.0", // Listen on all interfaces since it's public
		Port:              4222,
		NoAuthUser:        "",
		NoLog:             false, // Keep logs for monitoring
		NoSigs:            true,  // Disable signal handling since we handle it ourselves
		Logtime:           true,  // Include timestamps in logs
		Debug:             false, // Disable debug mode in production
		Trace:             false, // Disable trace mode in production
		TLSConfig:         serverTLSConfig,
		TLSHandshakeFirst: true,  // Require TLS handshake before any other communication
		AllowNonTLS:       false, // Only allow TLS connections
		TLSMap:            true,  // Enable TLS certificate mapping
		// Connection limits and DDoS protection
		MaxConn:          1000,            // Limit total connections
		MaxSubs:          100,             // Limit subscriptions per connection
		MaxControlLine:   4096,            // Limit control line size (4KB)
		MaxPayload:       512 * 1024,      // 512KB max payload for reports
		MaxPending:       1024 * 1024 * 2, // 2MB total pending messages
		MaxClosedClients: 1000,            // Keep track of closed clients
		// Rate limiting
		WriteDeadline: 1 * time.Second, // Timeout for write operations
		// Connection timeouts
		AuthTimeout:  2.0, // 2 seconds for auth
		TLSTimeout:   2.0, // 2 seconds for TLS handshake
		PingInterval: 2 * time.Second,
		MaxPingsOut:  3, // Disconnect after 3 missed pings
		// Security hardening
		NoHeaderSupport:     false, // Disable header support for simpler protocol
		NoFastProducerStall: true,  // Prevent fast producer stall
		// Graceful shutdown
		LameDuckDuration:    30 * time.Second,
		LameDuckGracePeriod: 10 * time.Second,
		// Define users with specific permissions
		Users: []*server.User{
			// User 1 with restricted permissions
			{
				Username: fmt.Sprintf("CN=%s,OU=%s,O=Chainlink Data Streams", clientPubHex[:32], clientPubHex),
				Permissions: &server.Permissions{
					Publish: &server.SubjectPermission{
						Allow: []string{"test.*"}, // Allows test.anything
					},
					Subscribe: &server.SubjectPermission{
						Allow: []string{"test.*"}, // Allows test.anything
					},
				},
			},
			// Insecure client with no permissions
		},
	}

	// Start the server
	ns, err := server.NewServer(opts)

	require.NoError(t, err)
	ns.Start()
	defer ns.Shutdown()

	// Wait for server to be ready
	for i := 0; i < 10; i++ {
		if ns.ReadyForConnections(1 * time.Second) {
			break
		}
		if i == 9 {
			t.Fatal("NATS server did not start in time")
		}
		time.Sleep(100 * time.Millisecond)
	}

	serverURL := fmt.Sprintf("tls://localhost:4222")

	testCases := []struct {
		name          string
		clientSigner  ed25519.PrivateKey
		serverPubKey  ed25519.PublicKey
		serverURLs    []string
		expectSuccess bool
		errorContains string
	}{
		{
			name:          "successful connection",
			clientSigner:  clientPriv,
			serverPubKey:  serverPub,
			serverURLs:    []string{serverURL},
			expectSuccess: true,
		},
		{
			name:          "invalid server URL",
			clientSigner:  clientPriv,
			serverPubKey:  serverPub,
			serverURLs:    []string{"tls://invalid:9999"},
			expectSuccess: false,
			errorContains: "failed to create NATS connection",
		},
		{
			name:          "wrong server public key",
			clientSigner:  clientPriv,
			serverPubKey:  make([]byte, ed25519.PublicKeySize), // Invalid key
			serverURLs:    []string{serverURL},
			expectSuccess: false,
			errorContains: "failed to create client mTLS credentials",
		},
	}

	clientOptions := ClientOpts{
		Logger:       logger.Test(t),
		ClientSigner: clientPriv,
		ServerPubKey: serverPub,
		ServerURLs:   []string{serverURL},
	}

	err = clientOptions.verifyConfig()
	require.NoError(t, err)

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {

			c, err := NewClient(clientOptions)

			servicetest.Run(t, c)
			require.NoError(t, err)

			if tc.expectSuccess {
				require.NoError(t, err)

				// Clean up connection
				err = c.Close()
				require.NoError(t, err)
			}
		})
	}
}

// // Helper to create a test NATS server with mTLS support
// func createTestServer(t *testing.T, allowedClientCerts []string) (*server.Server, string) {
// 	opts := &server.Options{
// 		Host:           "127.0.0.1",
// 		Port:           -1, // Use a random port
// 		NoLog:          true,
// 		NoSigs:         true,
// 		TLS:            true,
// 		TLSVerify:      true,
// 		TLSMap:         true,
// 		TLSTimeout:     5,
// 		TLSPinnedCerts: makePinnedCertSet(allowedClientCerts),
// 	}

// 	s, err := server.NewServer(opts)
// 	require.NoError(t, err)
// 	go s.Start()

// 	// Wait for server to be ready
// 	for i := 0; i < 10; i++ {
// 		if s.ReadyForConnections(1 * time.Second) {
// 			break
// 		}
// 		if i == 9 {
// 			t.Fatal("NATS server did not start in time")
// 		}
// 		time.Sleep(100 * time.Millisecond)
// 	}

// 	serverURL := fmt.Sprintf("tls://%s:%d", opts.Host, s.ClusterAddr().Port)
// 	return s, serverURL
// }

// // Helper function to convert string slice to PinnedCertSet
// func makePinnedCertSet(certs []string) server.PinnedCertSet {
// 	set := server.PinnedCertSet{}
// 	for _, cert := range certs {
// 		set[cert] = struct{}{}
// 	}
// 	return set
// }
