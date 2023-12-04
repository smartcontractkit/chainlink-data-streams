package llo

import (
	relayllo "github.com/smartcontractkit/chainlink-common/pkg/reportingplugins/llo"
)

var _ relayllo.ShouldRetireCache = &shouldRetireCache{}

type shouldRetireCache struct{}

func NewShouldRetireCache() relayllo.ShouldRetireCache {
	return newShouldRetireCache()
}

func newShouldRetireCache() *shouldRetireCache {
	return &shouldRetireCache{}
}

func (c *shouldRetireCache) ShouldRetire() (bool, error) {
	panic("TODO")
}
