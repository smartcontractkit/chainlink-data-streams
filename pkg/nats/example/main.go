package main

import (
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
	// Your custom TLS package
)

func startServer(opts *server.Options) (*server.Server, error) {
	// Create a new NATS server
	ns, err := server.NewServer(opts)
	if err != nil {
		return nil, fmt.Errorf("error creating server: %w", err)
	}

	// Start the server in a goroutine
	go func() {
		ns.Start()
	}()

	// Wait for the server to be ready
	if !ns.ReadyForConnections(4 * time.Second) {
		return nil, fmt.Errorf("NATS server failed to start")
	}

	log.Printf("NATS server is running on %s", ns.ClientURL())
	return ns, nil
}

func startClientandSendHello(clientName string, clientPriv ed25519.PrivateKey, serverPubKeys []ed25519.PublicKey, serverURL string) {
	// Set up client TLS config
	clientTLSSigner := crypto.Signer(clientPriv)
	clientTLSConfig, err := mtls.NewTLSTransportSigner(clientTLSSigner, serverPubKeys)
	if err != nil {
		log.Fatalf("Client %s: Error creating TLS config: %v", clientName, err)
	}

	natsOpts := []nats.Option{
		nats.Secure(clientTLSConfig),
		nats.ReconnectWait(500 * time.Millisecond),
		nats.Compression(true),
		nats.MaxReconnects(-1),
		nats.TLSHandshakeFirst(),
		nats.FlusherTimeout(1 * time.Second),
		nats.PingInterval(1 * time.Second),
		nats.Timeout(2 * time.Second),
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			log.Printf("Client %s: Error: %v", clientName, err)
		}),
		nats.Name(clientName),
	}
	nc, err := nats.Connect(serverURL, natsOpts...)
	if err != nil {
		log.Fatalf("Client %s: Error connecting to NATS: %v", clientName, err)
	}
	defer nc.Close()

	// Subscribe to the test subject
	sub, err := nc.Subscribe("test.*", func(msg *nats.Msg) {
		log.Printf("Client %s received: %s", clientName, string(msg.Data))
	})
	if err != nil {
		log.Fatalf("Client %s: Error subscribing: %v", clientName, err)
	}
	defer sub.Unsubscribe()

	// Publish a message
	err = nc.Publish("test.*", []byte(fmt.Sprintf("Hello world from %s", clientName)))
	if err != nil {
		log.Fatalf("Client %s: Error publishing message: %v", clientName, err)
	}

	// Ensure the publish message is sent to the server.
	if err := nc.Flush(); err != nil {
		log.Fatalf("Client %s: Error flushing connection: %v", clientName, err)
	}

	// Wait to ensure the message is received before closing.
	time.Sleep(2 * time.Second)
}

func main() {
	serverPub, serverPriv, _ := ed25519.GenerateKey(rand.Reader)
	log.Printf("Server public key: %x", serverPub)

	client1Pub, client1Priv, _ := ed25519.GenerateKey(rand.Reader)
	log.Printf("Client1 public key: %x", client1Pub)

	client2Pub, client2Priv, _ := ed25519.GenerateKey(rand.Reader)
	log.Printf("Client2 public key: %x", client2Pub)

	insecureClientPub, insecureClientPriv, _ := ed25519.GenerateKey(rand.Reader)
	log.Printf("Insecure client public key: %x", insecureClientPub)

	clientPubKeys := []ed25519.PublicKey{client1Pub, client2Pub}

	serverTLSConfig, err := mtls.NewTLSConfig(serverPriv, clientPubKeys)
	for _, key := range clientPubKeys {
		log.Printf("Client public key: %x", key)
	}

	if err != nil {
		log.Fatalf("Error creating TLS config: %v", err)
	}

	// Options block for nats-server.
	// NOTE: This structure is no longer used for monitoring endpoints
	// and json tags are deprecated and may be removed in the future.
	// Create an embedded NATS server with least privilege permissions

	client1PubHex := hex.EncodeToString(client1Pub)
	client2PubHex := hex.EncodeToString(client2Pub)
	// Create a new NATS server options
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
				Username: fmt.Sprintf("CN=%s,OU=%s,O=Chainlink Data Streams", client1PubHex[:32], client1PubHex),
				Permissions: &server.Permissions{
					Publish: &server.SubjectPermission{
						Allow: []string{"test.*"}, // Allows test.anything
					},
					Subscribe: &server.SubjectPermission{
						Allow: []string{"test.*"}, // Allows test.anything
					},
				},
			},

			// User 2 with different permissions
			{
				Username: fmt.Sprintf("CN=%s,OU=%s,O=Chainlink Data Streams", client2PubHex[:32], client2PubHex),
				Permissions: &server.Permissions{
					Publish: &server.SubjectPermission{
						Allow: []string{"other"},
						Deny:  []string{"service.admin.>", "service.internal.>"},
					},
					Subscribe: &server.SubjectPermission{
						Allow: []string{"other", "other"},
						Deny:  []string{"service.admin.>", "service.internal.>"},
					},
				},
			},

			// Insecure client with no permissions
		},
	}
	
	// Start the server
	ns, err := startServer(opts)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}

	serverPubKeys := []ed25519.PublicKey{serverPub}

	// Start client in a goroutine
	startClientandSendHello("client1", client1Priv, serverPubKeys, ns.ClientURL())
	startClientandSendHello("client2", client2Priv, serverPubKeys, ns.ClientURL())
	startClientandSendHello("insecureClient", insecureClientPriv, serverPubKeys, ns.ClientURL())

	// Handle graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	<-sigCh
	log.Println("\nShutting down...")
	ns.Shutdown()
}
