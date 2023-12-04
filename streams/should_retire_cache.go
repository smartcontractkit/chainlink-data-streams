package streams

var _ ShouldRetireCache = &shouldRetireCache{}

type shouldRetireCache struct{}

func NewShouldRetireCache() ShouldRetireCache {
	return newShouldRetireCache()
}

func newShouldRetireCache() *shouldRetireCache {
	return &shouldRetireCache{}
}

func (c *shouldRetireCache) ShouldRetire() (bool, error) {
	panic("TODO")
}
