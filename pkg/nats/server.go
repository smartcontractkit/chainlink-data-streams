package nats

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"fmt"
	"time"

	natssrv "github.com/nats-io/nats-server/v2/server"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
)

// Server is the interface for a NATS server service, mirroring the pattern used by the client.
type Server interface {
	services.Service

	// URL returns the server connect URL(s). Useful if you want to pass the
	// string to clients so they know how to connect.
	URL() []string
}

// Make sure *serverImpl implements Server.
var _ Server = (*serverImpl)(nil)

// serverImpl is the concrete implementation that holds the server instance and configuration.
type serverImpl struct {
	services.Service

	lggr logger.Logger
	opts ServerOpts
	srv  *natssrv.Server
	urls []string
}

// ServerOpts is the set of options required to stand up a NATS Server with mTLS.
type ServerOpts struct {
	// Logging
	Logger logger.Logger

	// The server’s private key. Must be an ed25519 key, used to prove identity to clients.
	ServerPrivKey ed25519.PrivateKey

	// Which client public keys are allowed to connect via mTLS. Typically includes
	// the set of ed25519.PublicKey(s) for the clients you'd like to authorize.
	AllowedClientPubKeys []ed25519.PublicKey

	// Where the server should listen (e.g. "0.0.0.0" or "127.0.0.1")
	Host string

	// The TCP port on which NATS server will listen (e.g. 4222).
	Port int

	// (Optional) Custom NATS permissions. If empty, this example code
	// defaults to some basic subject permissions.
	AllowedPublish   []string
	AllowedSubscribe []string
}

// NewServer constructs a new NATS Server service but does not start it. It mirrors
// the client pattern: we build a *serverImpl, then wrap it in a ServiceEngine so
// we get Start/Close/Health checks.
func NewServer(opts ServerOpts) (Server, error) {
	if err := verifyServerOpts(opts); err != nil {
		return nil, fmt.Errorf("invalid server configuration: %w", err)
	}

	s := &serverImpl{
		lggr: opts.Logger,
		opts: opts,
	}

	// Create a service lifecycle engine, using the same pattern as client.go.
	svc, err := services.Config{
		Name:  "NATSServer",
		Start: s.start,
		Close: s.close,
	}.NewServiceEngine(s.lggr)
	if err != nil {
		return nil, fmt.Errorf("failed to create NATSServer service engine: %w", err)
	}
	s.Service = svc

	return s, nil
}

// verifyServerOpts does basic checks on your server configuration to
// avoid returning an incorrectly configured server.
func verifyServerOpts(opts ServerOpts) error {
	if opts.Logger == nil {
		return fmt.Errorf("logger must not be nil")
	}
	if opts.ServerPrivKey == nil {
		return fmt.Errorf("server private key is required")
	}
	if len(opts.AllowedClientPubKeys) == 0 {
		return fmt.Errorf("at least one allowed client public key is required")
	}
	if opts.Host == "" {
		return fmt.Errorf("host must not be empty")
	}
	if opts.Port <= 0 {
		return fmt.Errorf("port must be > 0")
	}
	return nil
}

// start spins up the embedded NATS server with an mTLS config and begins listening.
func (s *serverImpl) start(ctx context.Context) error {
	// Build the server TLSConfig from the server’s private key & allowed client pub keys.
	serverTLSConfig, err := mtls.NewTLSConfig(s.opts.ServerPrivKey, s.opts.AllowedClientPubKeys)
	if err != nil {
		return fmt.Errorf("failed to create server TLS config: %w", err)
	}

	// Derive a default set of allowed subjects for testing or production usage.
	// If you'd like more advanced mapping, you can dynamically build natssrv.Users
	// with specific Permissions, etc.
	pubAllows := s.opts.AllowedPublish
	if len(pubAllows) == 0 {
		pubAllows = []string{"test.*"}
	}
	subAllows := s.opts.AllowedSubscribe
	if len(subAllows) == 0 {
		subAllows = []string{"test.*"}
	}

	// For each allowed client pubkey, build a user stanza with the same pattern
	// from your test code. This approach uses NATS TLSMap to tie the subject to
	// the client's certificate identity, so the client’s CN is typically:
	// "CN=<some string derived from pubkey hex>,OU=<full pubkey hex>,O=Chainlink Data Streams"
	// This is an example pattern. You can adapt it to your environment.
	var users []*natssrv.User
	for _, clientPub := range s.opts.AllowedClientPubKeys {
		clientHex := prettyKeyHex(clientPub)
		user := &natssrv.User{
			Username: fmt.Sprintf("CN=%s,OU=%s,O=Chainlink Data Streams",
				clientHex[:32], // maybe first 32 chars for brevity
				clientHex),
			Permissions: &natssrv.Permissions{
				Publish: &natssrv.SubjectPermission{
					Allow: pubAllows,
				},
				Subscribe: &natssrv.SubjectPermission{
					Allow: subAllows,
				},
			},
		}
		users = append(users, user)
	}

	// Configure the embedded server.
	natsOpts := &natssrv.Options{
		Host:              s.opts.Host,
		Port:              s.opts.Port,
		NoLog:             false,
		NoSigs:            true,
		Logtime:           true,
		Debug:             false,
		Trace:             false,
		TLSConfig:         serverTLSConfig,
		TLSHandshakeFirst: true,
		AllowNonTLS:       false,
		TLSMap:            true,
		// Connection limits & server resource constraints
		MaxConn:    1000,
		MaxSubs:    100,
		MaxPayload: 512 * 1024, // 512KB
		MaxPending: 2 * 1024 * 1024,
		// Timeouts
		WriteDeadline: 1 * time.Second,
		AuthTimeout:   2.0,
		TLSTimeout:    2.0,
		// Ping intervals
		PingInterval: 2 * time.Second,
		MaxPingsOut:  3,
		// Lame-duck
		LameDuckDuration:    30 * time.Second,
		LameDuckGracePeriod: 10 * time.Second,
		// User / permissions
		Users: users,
	}

	// Spin up the NATS server instance (this *does not* block).
	ns, err := natssrv.NewServer(natsOpts)
	if err != nil {
		return fmt.Errorf("failed to create embedded NATS server: %w", err)
	}

	// Start listening for connections in a goroutine. Because NATSServer doesn’t block on Start(),
	// you can proceed to do readiness checks to see if the server is ready for connections.
	go ns.Start()

	s.srv = ns
	// Prepare our final "URL()" that clients can connect to
	// e.g. "tls://HOST:PORT"
	addr := fmt.Sprintf("tls://%s:%d", s.opts.Host, s.opts.Port)
	s.urls = []string{addr}

	s.lggr.Info("NATS server is starting", "host", s.opts.Host, "port", s.opts.Port)
	return nil
}

// close gracefully shuts down the NATS server.
func (s *serverImpl) close() error {
	if s.srv == nil {
		return nil
	}
	s.lggr.Info("Shutting down NATS server", "host", s.opts.Host, "port", s.opts.Port)
	s.srv.Shutdown()
	return nil
}

// Healthy implements the Service interface and indicates if the NATS server
// is logically healthy (i.e. we have a non-nil server pointer).
func (s *serverImpl) Healthy() error {
	if s.srv == nil {
		return fmt.Errorf("NATS server is nil")
	}
	// Optionally, check internal server status or track metrics.
	return nil
}

// Ready implements the Service interface and indicates if the server is
// ready to accept connections. We can use the server's built-in readiness check.
func (s *serverImpl) Ready() error {
	if s.srv == nil {
		return fmt.Errorf("NATS server is nil")
	}
	// If we fail readiness (e.g. server not started or ephemeral failure), return an error.
	// 0-second wait means immediate check.
	if !s.srv.ReadyForConnections(0 * time.Second) {
		return fmt.Errorf("NATS server is not ready for connections")
	}
	return nil
}

// HealthReport aggregates the server's health in a map with the key as the
// service name.
func (s *serverImpl) HealthReport() map[string]error {
	return map[string]error{s.Name(): s.Healthy()}
}

// Name returns this service's name. If the logger has a name, use it. Otherwise "NATSServer".
func (s *serverImpl) Name() string {
	if s.lggr == nil {
		return "NATSServer"
	}
	return s.lggr.Name()
}

// URL returns the array of server URLs that clients can connect to, e.g. ["tls://localhost:4222"].
func (s *serverImpl) URL() []string {
	return s.urls
}

// prettyKeyHex helper function to turn ed25519.PublicKey or generic crypto.PublicKey into hex.
func prettyKeyHex(pub crypto.PublicKey) string {
	switch k := pub.(type) {
	case ed25519.PublicKey:
		return fmt.Sprintf("%x", k)
	default:
		return "unknown_public_key"
	}
}
