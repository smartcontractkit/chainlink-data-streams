package rpc

import (
	context "context"
	"crypto/ed25519"
	"testing"

	"github.com/smartcontractkit/chainlink-data-streams/rpc/mtls"
	"google.golang.org/grpc"
)

func TestClient(t *testing.T) {
	priv, pub := ed25519.GenerateKey(nil)

	c, _ := mtls.NewTransportCredentials(priv, []ed25519.PublicKey{pub})
	clientConn, _ := grpc.NewClient("", grpc.WithTransportCredentials(c))
	client := NewMercuryClient(clientConn)

	client.Transmit()
}

type server struct {
	*UnimplementedMercuryServer
}

func (s *server) Transmit(context.Context, *TransmitRequest) (*TransmitResponse, error) {

	return nil, nil
}
