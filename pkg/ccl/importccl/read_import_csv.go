// Copyright 2017 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package importccl

import (
	"context"
	"encoding/csv"
	"io"
	"runtime"

	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/ctxgroup"
	"github.com/cockroachdb/cockroach/pkg/util/tracing"
	"github.com/pkg/errors"
)

type csvInputReader struct {
	kvCh         chan kvBatch
	recordCh     chan csvRecord
	batchSize    int
	batch        csvRecord
	opts         roachpb.CSVOptions
	tableDesc    *sqlbase.TableDescriptor
	expectedCols int
}

var _ inputConverter = &csvInputReader{}

func newCSVInputReader(
	kvCh chan kvBatch, opts roachpb.CSVOptions, tableDesc *sqlbase.TableDescriptor, expectedCols int,
) *csvInputReader {
	return &csvInputReader{
		opts:         opts,
		kvCh:         kvCh,
		expectedCols: expectedCols,
		tableDesc:    tableDesc,
		recordCh:     make(chan csvRecord),
		batchSize:    500,
	}
}

func (c *csvInputReader) start(group ctxgroup.Group) {
	group.GoCtx(func(ctx context.Context) error {
		ctx, span := tracing.ChildSpan(ctx, "convertcsv")
		defer tracing.FinishSpan(span)

		defer close(c.kvCh)
		return ctxgroup.GroupWorkers(ctx, runtime.NumCPU(), func(ctx context.Context) error {
			return c.convertRecord(ctx)
		})
	})
}

func (c *csvInputReader) inputFinished() {
	close(c.recordCh)
}

func (c *csvInputReader) flushBatch(ctx context.Context, finished bool, progFn progressFn) error {
	// if the batch isn't empty, we need to flush it.
	if len(c.batch.r) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case c.recordCh <- c.batch:
		}
	}
	if progressErr := progFn(finished); progressErr != nil {
		return progressErr
	}
	if !finished {
		c.batch.r = make([][]string, 0, c.batchSize)
	}
	return nil
}

func (c *csvInputReader) readFile(
	ctx context.Context, input io.Reader, inputIdx int32, inputName string, progressFn progressFn,
) error {
	cr := csv.NewReader(input)
	if c.opts.Comma != 0 {
		cr.Comma = c.opts.Comma
	}
	cr.FieldsPerRecord = -1
	cr.LazyQuotes = true
	cr.Comment = c.opts.Comment

	c.batch = csvRecord{
		file:      inputName,
		fileIndex: inputIdx,
		rowOffset: 1,
		r:         make([][]string, 0, c.batchSize),
	}

	for i := 1; ; i++ {
		record, err := cr.Read()
		finished := err == io.EOF
		if finished || len(c.batch.r) >= c.batchSize {
			if err := c.flushBatch(ctx, finished, progressFn); err != nil {
				return err
			}
			c.batch.rowOffset = i
		}
		if finished {
			break
		}
		if err != nil {
			return errors.Wrapf(err, "row %d: reading CSV record", i)
		}
		// Ignore the first N lines.
		if uint32(i) <= c.opts.Skip {
			continue
		}
		if len(record) == c.expectedCols {
			// Expected number of columns.
		} else if len(record) == c.expectedCols+1 && record[c.expectedCols] == "" {
			// Line has the optional trailing comma, ignore the empty field.
			record = record[:c.expectedCols]
		} else {
			return errors.Errorf("row %d: expected %d fields, got %d", i, c.expectedCols, len(record))
		}
		c.batch.r = append(c.batch.r, record)
	}
	return nil
}

type csvRecord struct {
	r         [][]string
	file      string
	fileIndex int32
	rowOffset int
}

// convertRecord converts CSV records into KV pairs and sends them on the
// kvCh chan.
func (c *csvInputReader) convertRecord(ctx context.Context) error {
	conv, err := newRowConverter(c.tableDesc, c.kvCh)
	if err != nil {
		return err
	}
	if conv.evalCtx.SessionData == nil {
		panic("uninitialized session data")
	}

	for batch := range c.recordCh {
		for batchIdx, record := range batch.r {
			rowNum := int64(batch.rowOffset + batchIdx)
			for i, v := range record {
				col := conv.visibleCols[i]
				if c.opts.NullEncoding != nil && v == *c.opts.NullEncoding {
					conv.datums[i] = tree.DNull
				} else {
					var err error
					conv.datums[i], err = tree.ParseDatumStringAs(col.Type.ToDatumType(), v, &conv.evalCtx)
					if err != nil {
						return errors.Wrapf(err, "%s: row %d: parse %q as %s", batch.file, rowNum, col.Name, col.Type.SQLString())
					}
				}
			}
			if err := conv.row(ctx, batch.fileIndex, rowNum); err != nil {
				return errors.Wrapf(err, "converting row: %s: row %d", batch.file, rowNum)
			}
		}
	}
	return conv.sendBatch(ctx)
}
