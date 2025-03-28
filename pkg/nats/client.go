package nats

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
	"github.com/smartcontractkit/chainlink-data-streams/rpc"
	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
)

type Client interface {
	services.Service
	Transmit(ctx context.Context, payload []byte, dedupKey string, donID uint32) error
}

var _ Client = (*client)(nil)

type client struct {
	services.Service

	lggr            logger.Logger
	clientSigner    crypto.Signer
	serverPubKey    ed25519.PublicKey
	serverURLs      []string
	clientPubKeyHex string

	conn *nats.Conn
	js   nats.JetStreamContext
}

func NewClient(opts ClientOpts) (Client, error) {
	if err := opts.verifyConfig(); err != nil {
		return nil, fmt.Errorf("invalid configuration: %w", err)
	}

	c := &client{
		lggr:         opts.Logger,
		clientSigner: opts.ClientSigner,
		serverPubKey: opts.ServerPubKey,
		serverURLs:   opts.ServerURLs,
	}

	svc, _ := services.Config{
		Name:  "NATSClient",
		Start: c.start,
		Close: c.close,
	}.NewServiceEngine(opts.Logger)
	c.Service = svc

	return c, nil
}

// connect creates a new NATS connection with the given configuration
func (c *client) connect() (*nats.Conn, error) {
	cMtls, err := mtls.NewTLSTransportSigner(c.clientSigner, []ed25519.PublicKey{c.serverPubKey})
	if err != nil {
		return nil, fmt.Errorf("failed to create client mTLS credentials: %w", err)
	}
	options := []nats.Option{
		// Connection settings
		nats.ReconnectWait(1 * time.Second),
		nats.RetryOnFailedConnect(true),
		nats.MaxReconnects(-1),
		nats.ReconnectBufSize(256 * 1024 * 1024), // 256MB
		// Timeouts and keepalive
		nats.PingInterval(1 * time.Second),
		nats.Timeout(5 * time.Second),
		nats.TLSHandshakeFirst(),
		nats.Secure(cMtls),
		nats.Name(c.getClientPubKeyHex()),
		// Connection handlers for various NATS events
		nats.ConnectHandler(func(nc *nats.Conn) {
			c.lggr.Info("NATS client connection established", "server_id", nc.ConnectedServerId(), "server_url", nc.ConnectedUrl())
		}),
		nats.ReconnectHandler(func(nc *nats.Conn) {
			c.lggr.Info("NATS client reconnected", nc.ConnectedServerId(), "server_url", nc.ConnectedUrl(), "total_reconnects", nc.Reconnects)
		}),
		nats.ReconnectErrHandler(func(nc *nats.Conn, err error) {
			c.lggr.Error("NATS client reconnected with error", nc.ConnectedServerId(), "server_url", nc.ConnectedUrl(), "error", err)
		}),
		nats.DisconnectErrHandler(func(nc *nats.Conn, err error) {
			c.lggr.Error("NATS client disconnected with error", nc.ConnectedServerId(), "server_url", nc.ConnectedUrl(), "total_reconnects", nc.Reconnects, "error", err)
		}),
		nats.ClosedHandler(func(nc *nats.Conn) {
			c.lggr.Warn("NATS client closed", "server_id", nc.ConnectedServerId(), "server_url", nc.ConnectedUrl())
		}),
		// Error handler for subscriptions
		nats.ErrorHandler(func(nc *nats.Conn, sub *nats.Subscription, err error) {
			c.lggr.Error("NATS client subscription error", "server_id", nc.ConnectedServerId(), "server_url", nc.ConnectedUrl(), "error", err, "subject", sub.Subject, "queue", sub.Queue)
		}),
	}

	nc, err := nats.Connect(strings.Join(c.serverURLs, ","), options...)
	if err != nil {
		return nil, fmt.Errorf("failed to create NATS connection: %w", err)
	}

	jsOptions := []nats.JSOpt{
		nats.PublishAsyncMaxPending(256 * 1024), // Allow large number of async publishes
		nats.PublishAsyncTimeout(100 * time.Millisecond),
		nats.MaxWait(100 * time.Millisecond),
	}

	// Create the JetStream context
	c.js, err = nc.JetStream(jsOptions...)
	if err != nil {
		return nil, fmt.Errorf("failed to create JetStream context: %w", err)
	}

	return nc, nil
}

func (c *client) start(context.Context) error {
	nc, err := c.connect()
	// if there is only one server URL, use it
	if err != nil {
		return err
	}
	c.conn = nc
	return nil
}

func (c *client) close() error {
	if c.conn != nil {
		return c.conn.Drain()
	}
	return nil
}

func (c *client) Transmit(ctx context.Context, payload []byte, dedupKey string, donID uint32) error {
	subject := fmt.Sprintf("%d.%s", donID, c.getClientPubKeyHex())

	pubOpts := []nats.PubOpt{
		nats.MsgId(dedupKey),
		nats.StallWait(200 * time.Millisecond),
		nats.MsgTTL(24 * time.Hour),
	}
	ack, err := c.js.PublishAsync(subject, payload, pubOpts...)
	if err != nil {
		return fmt.Errorf("failed to publish to fast lane: %w", err)
	}
	select {
	case <-ack.Ok():
		return nil
	case <-time.After(1 * time.Second):
		return fmt.Errorf("fast lane publish timed out")
	}
}

func (c *client) LatestReport(ctx context.Context, req *rpc.LatestReportRequest) (resp *rpc.LatestReportResponse, err error) {
	return nil, errors.New("LatestReport is not supported in nats mode")
}

func (c *client) Name() string {
	if c.lggr == nil {
		return "NATSClient"
	}
	return c.lggr.Name()
}

func (c *client) Healthy() error {
	switch {
	case c.conn == nil:
		return fmt.Errorf("NATS connection is nil")
	case !c.conn.IsConnected():
		return fmt.Errorf("NATS connection is %s", c.conn.Status())
	default:
		return nil
	}
}

func (c *client) Ready() error {
	if c.conn == nil || !c.conn.IsConnected() {
		return errors.New("NATS connection is not ready")
	}
	return nil
}

func (c *client) HealthReport() map[string]error {
	return map[string]error{c.Name(): c.Healthy()}
}

func (c *client) getClientPubKeyHex() string {
	return hex.EncodeToString(c.clientSigner.Public().(ed25519.PublicKey))
}
