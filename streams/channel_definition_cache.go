package streams

import (
	"context"
	"maps"

	"github.com/smartcontractkit/chainlink-common/pkg/logger"
	"github.com/smartcontractkit/chainlink-common/pkg/services"
)

// TODO: needs to be populated asynchronously from onchain ConfigurationStore
type ChannelDefinitionCache interface {
	// TODO: Would this necessarily need to be scoped by contract address?
	Definitions() ChannelDefinitions
	services.Service
}

var _ ChannelDefinitionCache = &channelDefinitionCache{}

type channelDefinitionCache struct {
	services.StateMachine

	lggr        logger.Logger
	definitions ChannelDefinitions
}

func NewChannelDefinitionCache() ChannelDefinitionCache {
	return &channelDefinitionCache{}
}

// TODO: Needs a way to subscribe/unsubscribe to contracts

func (c *channelDefinitionCache) Start(ctx context.Context) error {
	// TODO: Initial load, then poll
	// TODO: needs to be populated asynchronously from onchain ConfigurationStore
	return nil
}

func (c *channelDefinitionCache) Close() error {
	// TODO
	return nil
}

func (c *channelDefinitionCache) HealthReport() map[string]error {
	report := map[string]error{c.Name(): c.Healthy()}
	return report
}

func (c *channelDefinitionCache) Name() string { return c.lggr.Name() }

func (c *channelDefinitionCache) Definitions() ChannelDefinitions {
	c.StateMachine.RLock()
	defer c.StateMachine.RUnlock()
	return maps.Clone(c.definitions)
}
