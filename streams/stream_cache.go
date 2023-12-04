package llo

import (
	"context"

	relayllo "github.com/smartcontractkit/chainlink-common/pkg/reportingplugins/llo"
	"github.com/smartcontractkit/chainlink/v2/core/services/pg"
)

type StreamCache interface {
	Get(streamID relayllo.StreamID) (Stream, bool)
}

type streamCache struct {
	q       pg.Queryer
	streams map[relayllo.StreamID]Stream
}

func NewStreamCache(q pg.Queryer) StreamCache {
	return &streamCache{
		q,
		make(map[relayllo.StreamID]Stream),
	}
}

func (s *streamCache) Get(streamID relayllo.StreamID) (Stream, bool) {
	strm, exists := s.streams[streamID]
	return strm, exists
}

func (s *streamCache) Load(ctx context.Context) error {
	rows, err := s.q.QueryContext(ctx, "SELECT s.id, ps.id, ps.dot_dag_source, ps.max_task_duration FROM streams s JOIN pipeline_specs ps ON ps.id = s.pipeline_spec_id")
	if err != nil {
		// TODO: retries?
		return err
	}

	for rows.Next() {
		var strm stream
		if err := rows.Scan(&strm.id, &strm.spec.ID, &strm.spec.DotDagSource, &strm.spec.MaxTaskDuration); err != nil {
			return err
		}
		s.streams[strm.id] = &strm
	}
	return rows.Err()
}
