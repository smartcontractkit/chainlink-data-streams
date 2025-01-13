package rpc

import (
	"context"
	"crypto/ed25519"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/keepalive"

	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
)

func TestClient(t *testing.T) {
	spub, spriv, err := ed25519.GenerateKey(nil)
	assert.NoError(t, err)
	cpub, cpriv, err := ed25519.GenerateKey(nil)
	assert.NoError(t, err)

	sMtls, err := mtls.NewTransportCredentials(spriv, []ed25519.PublicKey{cpub})
	assert.NoError(t, err)
	s := grpc.NewServer(grpc.Creds(sMtls))
	srv := &server{}
	RegisterMercuryServer(s, srv)
	conn, err := net.Listen("tcp", "127.0.0.1:8080")
	assert.NoError(t, err)
	go func() {
		sErr := s.Serve(conn)
		assert.True(t, errors.Is(sErr, grpc.ErrServerStopped))
	}()

	cMtls, err := mtls.NewTransportCredentials(cpriv, []ed25519.PublicKey{spub})
	assert.NoError(t, err)
	clientConn, err := grpc.NewClient(
		"127.0.0.1:8080",
		grpc.WithTransportCredentials(cMtls),
		grpc.WithConnectParams(
			grpc.ConnectParams{
				Backoff: backoff.Config{
					BaseDelay:  1.0 * time.Second,
					Multiplier: 1.6,
					Jitter:     0.2,
					MaxDelay:   120 * time.Second,
				},
				MinConnectTimeout: time.Second,
			},
		),
		grpc.WithKeepaliveParams(
			keepalive.ClientParameters{
				Time:                time.Second * 10,
				Timeout:             time.Second * 20,
				PermitWithoutStream: true,
			}),
		grpc.WithDefaultCallOptions(
			grpc.WaitForReady(true),
		),
	)
	assert.NoError(t, err)
	client := NewMercuryClient(clientConn)

	r, err := client.Transmit(context.Background(), &TransmitRequest{})
	assert.NoError(t, err)

	assert.NotNil(t, r)
}

type server struct {
	UnimplementedMercuryServer
}

func (s *server) Transmit(context.Context, *TransmitRequest) (*TransmitResponse, error) {
	return &TransmitResponse{}, nil
}