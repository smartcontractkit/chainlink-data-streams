package nats

import (
	"crypto"
	"crypto/ed25519"
	"fmt"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
)

type ClientOpts struct {
	Logger       logger.Logger
	ClientSigner crypto.Signer
	ServerPubKey ed25519.PublicKey
	ServerURLs   []string
}

// verifyConfig validates all required fields are properly set
func (c *ClientOpts) verifyConfig() error {
	var errs []error

	if c.Logger == nil {
		errs = append(errs, fmt.Errorf("logger is required for NATS client"))
	}
	if c.ClientSigner == nil {
		errs = append(errs, fmt.Errorf("client signer is required for NATS client"))
	}
	if len(c.ServerPubKey) == 0 {
		errs = append(errs, fmt.Errorf("server public key is required for NATS client"))
	}
	if len(c.ServerURLs) == 0 {
		errs = append(errs, fmt.Errorf("at least one server URL is required for NATS client"))
	}

	if len(errs) > 0 {
		return fmt.Errorf("invalid NATS client configuration: %v", errs)
	}

	return nil
}
