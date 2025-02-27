package connkafka

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/twmb/franz-go/pkg/kgo"
	lua "github.com/yuin/gopher-lua"

	"github.com/PeerDB-io/peer-flow/connectors/utils"
	"github.com/PeerDB-io/peer-flow/generated/protos"
	"github.com/PeerDB-io/peer-flow/model"
	"github.com/PeerDB-io/peer-flow/pua"
)

func (*KafkaConnector) SetupQRepMetadataTables(_ context.Context, _ *protos.QRepConfig) error {
	return nil
}

func (c *KafkaConnector) SyncQRepRecords(
	ctx context.Context,
	config *protos.QRepConfig,
	partition *protos.QRepPartition,
	stream *model.QRecordStream,
) (int, error) {
	startTime := time.Now()
	numRecords := atomic.Int64{}
	schema := stream.Schema()

	shutdown := utils.HeartbeatRoutine(ctx, func() string {
		return fmt.Sprintf("sent %d records to %s", numRecords.Load(), config.DestinationTableIdentifier)
	})
	defer shutdown()

	queueCtx, queueErr := context.WithCancelCause(ctx)
	pool, err := c.createPool(queueCtx, config.Script, config.FlowJobName, nil, queueErr)
	if err != nil {
		return 0, err
	}
	defer pool.Close()

Loop:
	for {
		select {
		case qrecord, ok := <-stream.Records:
			if !ok {
				c.logger.Info("flushing batches because no more records")
				break Loop
			}

			pool.Run(func(ls *lua.LState) poolResult {
				items := model.NewRecordItems(len(qrecord))
				for i, val := range qrecord {
					items.AddColumn(schema.Fields[i].Name, val)
				}
				record := &model.InsertRecord[model.RecordItems]{
					BaseRecord:           model.BaseRecord{},
					Items:                items,
					SourceTableName:      config.WatermarkTable,
					DestinationTableName: config.DestinationTableIdentifier,
					CommitID:             0,
				}

				lfn := ls.Env.RawGetString("onRecord")
				fn, ok := lfn.(*lua.LFunction)
				if !ok {
					queueErr(fmt.Errorf("script should define `onRecord` as function, not %s", lfn))
					return poolResult{}
				}

				ls.Push(fn)
				ls.Push(pua.LuaRecord.New(ls, record))
				err := ls.PCall(1, -1, nil)
				if err != nil {
					queueErr(fmt.Errorf("script failed: %w", err))
					return poolResult{}
				}

				args := ls.GetTop()
				results := make([]*kgo.Record, 0, args)
				for i := range args {
					kr, err := lvalueToKafkaRecord(ls, ls.Get(i-args))
					if err != nil {
						queueErr(err)
						return poolResult{}
					}
					if kr != nil {
						if kr.Topic == "" {
							kr.Topic = record.GetDestinationTableName()
						}
						results = append(results, kr)
					}
				}
				ls.SetTop(0)
				numRecords.Add(1)
				return poolResult{records: results}
			})

		case <-queueCtx.Done():
			break Loop
		}
	}

	if err := pool.Wait(queueCtx); err != nil {
		return 0, err
	}
	if err := c.client.Flush(queueCtx); err != nil {
		return 0, fmt.Errorf("[kafka] final flush error: %w", err)
	}

	if err := c.FinishQRepPartition(ctx, partition, config.FlowJobName, startTime); err != nil {
		return 0, err
	}
	return int(numRecords.Load()), nil
}
