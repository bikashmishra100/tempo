package v2

import (
	"bytes"
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/grafana/tempo/tempodb/backend"
	"github.com/grafana/tempo/tempodb/encoding/common"
)

type StreamingBlock struct {
	compactedMeta *backend.BlockMeta
	inMetas       []*backend.BlockMeta

	bloom *common.ShardedBloomFilter

	bufferedObjects int
	appendBuffer    *bytes.Buffer
	appender        Appender

	cfg *common.BlockConfig
}

// NewStreamingBlock creates a ... new streaming block. Objects are appended one at a time to the backend.
func NewStreamingBlock(cfg *common.BlockConfig, id uuid.UUID, tenantID string, metas []*backend.BlockMeta, estimatedObjects int) (*StreamingBlock, error) {
	if len(metas) == 0 {
		return nil, fmt.Errorf("empty block meta list")
	}

	dataEncoding := metas[0].DataEncoding
	for _, meta := range metas {
		if meta.DataEncoding != dataEncoding {
			return nil, fmt.Errorf("two blocks of different data encodings can not be streamed together: %s: %s", dataEncoding, meta.DataEncoding)
		}
	}

	c := &StreamingBlock{
		compactedMeta: backend.NewBlockMeta(tenantID, id, versionString, cfg.Encoding, dataEncoding),
		bloom:         common.NewBloom(cfg.BloomFP, uint(cfg.BloomShardSizeBytes), uint(estimatedObjects)),
		inMetas:       metas,
		cfg:           cfg,
	}

	c.appendBuffer = &bytes.Buffer{}
	dataWriter, err := NewDataWriter(c.appendBuffer, cfg.Encoding)
	if err != nil {
		return nil, fmt.Errorf("failed to create page writer: %w", err)
	}

	c.appender, err = NewBufferedAppender(dataWriter, cfg.IndexDownsampleBytes, estimatedObjects)
	if err != nil {
		return nil, fmt.Errorf("failed to created appender: %w", err)
	}

	return c, nil
}

func (c *StreamingBlock) AddObject(id common.ID, object []byte) error {
	err := c.appender.Append(id, object)
	if err != nil {
		return err
	}
	c.bufferedObjects++
	c.compactedMeta.ObjectAdded(id, 0, 0) // streaming block handles start/end time by combining BlockMetas. See .BlockMeta()
	c.bloom.Add(id)
	return nil
}

func (c *StreamingBlock) CurrentBufferLength() int {
	return c.appendBuffer.Len()
}

func (c *StreamingBlock) CurrentBufferedObjects() int {
	return c.bufferedObjects
}

func (c *StreamingBlock) Length() int {
	return c.appender.Length()
}

// FlushBuffer flushes any existing objects to the backend
func (c *StreamingBlock) FlushBuffer(ctx context.Context, tracker backend.AppendTracker, w backend.Writer) (backend.AppendTracker, int, error) {
	if c.appender.Length() == 0 {
		return tracker, 0, nil
	}

	meta := c.BlockMeta()
	tracker, err := appendBlockData(ctx, w, meta, tracker, c.appendBuffer.Bytes())
	if err != nil {
		return nil, 0, err
	}

	bytesFlushed := c.appendBuffer.Len()
	c.appendBuffer.Reset()
	c.bufferedObjects = 0

	return tracker, bytesFlushed, nil
}

// Complete finishes writes the compactor metadata and closes all buffers and appenders
func (c *StreamingBlock) Complete(ctx context.Context, tracker backend.AppendTracker, w backend.Writer) (int, error) {
	err := c.appender.Complete()
	if err != nil {
		return 0, err
	}

	// one final flush
	tracker, bytesFlushed, err := c.FlushBuffer(ctx, tracker, w)
	if err != nil {
		return 0, err
	}

	// close data file
	err = w.CloseAppend(ctx, tracker)
	if err != nil {
		return 0, err
	}

	records := c.appender.Records()
	meta := c.BlockMeta()

	indexWriter := NewIndexWriter(c.cfg.IndexPageSizeBytes)
	indexBytes, err := indexWriter.Write(records)
	if err != nil {
		return 0, err
	}

	meta.TotalRecords = uint32(len(records)) // casting
	meta.IndexPageSize = uint32(c.cfg.IndexPageSizeBytes)
	meta.BloomShardCount = uint16(c.bloom.GetShardCount())

	return bytesFlushed, writeBlockMeta(ctx, w, meta, indexBytes, c.bloom)
}

func (c *StreamingBlock) BlockMeta() *backend.BlockMeta {
	meta := c.compactedMeta

	meta.StartTime = c.inMetas[0].StartTime
	meta.EndTime = c.inMetas[0].EndTime
	meta.Size = c.appender.DataLength()

	// everything should be correct here except the start/end times which we will get from the passed in metas
	for _, m := range c.inMetas[1:] {
		if m.StartTime.Before(meta.StartTime) {
			meta.StartTime = m.StartTime
		}
		if m.EndTime.After(meta.EndTime) {
			meta.EndTime = m.EndTime
		}
	}

	return meta
}
