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
	"path/filepath"
	"syscall"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
	// Your custom TLS package
)

func loadOrGenerateKeys(keysDir, prefix string) (ed25519.PublicKey, ed25519.PrivateKey) {
	// Generate or load server keys
	serverPubPath := filepath.Join(keysDir, prefix+".pub")
	serverPrivPath := filepath.Join(keysDir, prefix+".priv")

	var serverPub ed25519.PublicKey
	var serverPriv ed25519.PrivateKey

	// Try to load existing keys
	pubBytes, err := os.ReadFile(serverPubPath)
	if err == nil {
		privBytes, err := os.ReadFile(serverPrivPath)
		if err == nil {
			// Decode existing keys
			pubBytes, err = hex.DecodeString(string(pubBytes))
			if err != nil {
				log.Fatalf("Error decoding server public key: %v", err)
			}
			privBytes, err = hex.DecodeString(string(privBytes))
			if err != nil {
				log.Fatalf("Error decoding server private key: %v", err)
			}
			serverPub = ed25519.PublicKey(pubBytes)
			serverPriv = ed25519.PrivateKey(privBytes)
		}
	}

	// Generate new keys if they don't exist
	if serverPub == nil || serverPriv == nil {
		var err error
		serverPub, serverPriv, err = ed25519.GenerateKey(rand.Reader)
		if err != nil {
			log.Fatalf("Error generating server keys: %v", err)
		}

		// Save new keys
		err = os.WriteFile(serverPubPath, []byte(hex.EncodeToString(serverPub)), 0644)
		if err != nil {
			log.Fatalf("Error saving server public key: %v", err)
		}
		err = os.WriteFile(serverPrivPath, []byte(hex.EncodeToString(serverPriv)), 0600)
		if err != nil {
			log.Fatalf("Error saving server private key: %v", err)
		}
	}

	return serverPub, serverPriv
}

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
	}
	nc, err := nats.Connect(serverURL, natsOpts...)
	if err != nil {
		log.Fatalf("Client %s: Error connecting to NATS: %v", clientName, err)
	}
	defer nc.Close()

	// Subscribe to the test subject
	sub, err := nc.Subscribe("test", func(msg *nats.Msg) {
		log.Printf("Client %s received: %s", clientName, string(msg.Data))
	})
	if err != nil {
		log.Fatalf("Client %s: Error subscribing: %v", clientName, err)
	}
	defer sub.Unsubscribe()

	// Send a message
	err = nc.Publish("test", []byte(fmt.Sprintf("Hello world from %s", clientName)))
	if err != nil {
		log.Fatalf("Client %s: Error publishing message: %v", clientName, err)
	}

	log.Printf("Client %s: Message published successfully", clientName)

	// Keep the connection alive
	defer nc.Close()
}

func main() {
	serverPub, serverPriv := loadOrGenerateKeys("keys", "server")
	log.Printf("Server public key: %x", serverPub)
	log.Printf("Server private key: %x", serverPriv)

	client1Pub, client1Priv := loadOrGenerateKeys("keys", "client")
	log.Printf("Client public key: %x", client1Pub)
	log.Printf("Client private key: %x", client1Priv)

	client2Pub, client2Priv := loadOrGenerateKeys("keys", "client2")
	log.Printf("Client2 public key: %x", client2Pub)
	log.Printf("Client2 private key: %x", client2Priv)

	insecureClientPub, insecureClientPriv := loadOrGenerateKeys("keys", "insecureClient")
	log.Printf("Insecure client public key: %x", insecureClientPub)
	log.Printf("Insecure client private key: %x", insecureClientPriv)

	fmt.Printf("Client1 username (CN): %s\n", hex.EncodeToString(client1Pub))
	fmt.Printf("Client2 username (CN): %s\n", hex.EncodeToString(client2Pub))

	clientPubKeys := []ed25519.PublicKey{client1Pub, client2Pub}

	serverTLSConfig, err := mtls.NewTLSConfig(serverPriv, clientPubKeys)
	for _, key := range clientPubKeys {
		log.Printf("Client public key: %x", key)
	}

	if err != nil {
		log.Fatalf("Error creating TLS config: %v", err)
	}

	// Create a new NATS server options
	opts := &server.Options{
		Host:              "0.0.0.0", // Listen on all interfaces since it's public
		Port:              4222,
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
		NoHeaderSupport:     true, // Disable header support for simpler protocol
		NoFastProducerStall: true, // Prevent fast producer stall
		// Graceful shutdown
		LameDuckDuration:    30 * time.Second,
		LameDuckGracePeriod: 10 * time.Second,
		// Users with permissions
		Users: []*server.User{
			{
				Username: fmt.Sprintf("OU=%s,O=%s", hex.EncodeToString(client1Pub), hex.EncodeToString(client1Pub)),
				Permissions: &server.Permissions{
					Publish:   &server.SubjectPermission{Allow: []string{"test.*"}},
					Subscribe: &server.SubjectPermission{Allow: []string{"test.*"}},
				},
			},
			{
				Username: fmt.Sprintf("OU=%s,O=%s", hex.EncodeToString(client2Pub), hex.EncodeToString(client2Pub)),
				Permissions: &server.Permissions{
					Publish:   &server.SubjectPermission{Allow: []string{"else.*"}},
					Subscribe: &server.SubjectPermission{Allow: []string{"else.*"}},
				},
			},
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
