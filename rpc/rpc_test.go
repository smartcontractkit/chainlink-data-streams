package rpc

import (
	context "context"
	"crypto/ed25519"
	"net"
	"testing"

	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
	"github.com/stretchr/testify/assert"
	"google.golang.org/grpc"
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
		err := s.Serve(conn)
		if err != grpc.ErrServerStopped {
			assert.NoError(t, err)
		}
	}()

	cMtls, err := mtls.NewTransportCredentials(cpriv, []ed25519.PublicKey{spub})
	assert.NoError(t, err)
	clientConn, err := grpc.NewClient("127.0.0.1:8080", grpc.WithTransportCredentials(cMtls))
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
