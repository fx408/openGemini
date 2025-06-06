// Copyright 2025 Huawei Cloud Computing Technologies Co., Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package shelf_test

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/openGemini/openGemini/engine/shelf"
	"github.com/openGemini/openGemini/lib/config"
	"github.com/openGemini/openGemini/lib/errno"
	"github.com/openGemini/openGemini/lib/record"
	"github.com/openGemini/openGemini/lib/util"
	"github.com/stretchr/testify/require"
)

func TestWal(t *testing.T) {
	runWalTest(t)

	conf := &config.GetStoreConfig().ShelfMode

	// lz4 compress
	conf.WalCompressMode = 1
	runWalTest(t)
	conf.WalCompressMode = 0

	// snappy compress
	conf.WalCompressMode = 2
	runWalTest(t)
	conf.WalCompressMode = 0
}

func runWalTest(t *testing.T) {
	sid := uint64(100)
	row := buildRow(1, "foo", 10)
	row.UnmarshalIndexKeys(nil)
	rec := buildRecord(10, 88)
	times := append([]int64{}, rec.Times()...)
	lock := ""

	wal := shelf.NewWal(t.TempDir(), &lock, "foo")
	wal.BackgroundSync()

	err := wal.WriteRecord(sid, row.IndexKey, rec)
	require.NoError(t, err)

	err = wal.WriteRecord(sid, row.IndexKey, rec)
	require.NoError(t, err)
	require.NoError(t, wal.Sync())
	wal.BackgroundSync()

	require.False(t, wal.SizeLimited())

	other := &record.Record{}
	err = wal.ReadRecord(&shelf.WalCtx{}, sid, other)
	require.NoError(t, err)
	record.CheckRecord(other)

	require.Equal(t, rec.Schema, other.Schema)
	require.Equal(t, times[0], other.Times()[0])
	wal.MustClose()
}

func TestLZ4CompressBlock(t *testing.T) {
	data := []byte("foo1,foo2,foo1,foo2,foo1,foo2")
	buf := make([]byte, 16)
	headerSize := 16

	block, err := shelf.LZ4CompressBlock(data, buf)
	require.NoError(t, err)

	other, err := shelf.LZ4DecompressBlock(block[headerSize:], buf)
	require.NoError(t, err)
	require.Equal(t, data, other)

	binary.BigEndian.PutUint32(block[headerSize:], 10)
	_, err = shelf.LZ4DecompressBlock(block[headerSize:], buf)
	require.NotEmpty(t, err)
}

func TestSnappyCompressBlock(t *testing.T) {
	data := []byte("foo1,foo2,foo1,foo2,foo1,foo2")
	buf := make([]byte, 16)
	headerSize := 16

	block := shelf.SnappyCompressBlock(data, buf)

	other, err := shelf.SnappyDecompressBlock(block[headerSize:], buf)
	require.NoError(t, err)
	require.Equal(t, data, other)
}

func TestLoadWalFiles(t *testing.T) {
	lock := ""
	dir := t.TempDir()
	shard, idx, store := newShard(10, dir)
	idx.sidCache = 0
	idx.sidCreate = 0
	walDir := shard.GetWalDir()

	row := buildRow(1, "foo", 10)
	row.UnmarshalIndexKeys(nil)

	wal := shelf.NewWal(walDir, &lock, "foo")

	var data = make(map[uint64]*record.Record)
	for i := range uint64(100) {
		rec := buildRecord(10, int(i*77+100))

		id := i
		exp, ok := data[id]
		if ok {
			exp.Merge(rec)
		} else {
			exp = &record.Record{}
			exp.Schema = append(exp.Schema[:0], rec.Schema...)
			exp.ReserveColVal(rec.Len())
			exp.AppendRec(rec, 0, rec.RowNums())
			data[id] = exp
		}

		err := wal.WriteRecord(id, row.IndexKey, rec)
		require.NoError(t, err)
	}

	wal.MustClose()

	require.NoError(t, os.MkdirAll(filepath.Join(walDir, "redis_0000"), 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(walDir, "cpu_0000"), 0700))

	idx.sidCache = 1000
	idx.sidCreate = 1000
	shard.Load()
	shard.Wait()
	shard.Stop()

	data[1000] = data[0]

	require.True(t, len(store.files) > 0)
	for _, f := range store.files {
		itrTSSPFile(f, func(sid uint64, rec *record.Record) {
			record.CheckRecord(rec)

			exp, ok := data[sid]
			require.True(t, ok)

			require.Equal(t, exp.Times(), rec.Times())
		})
		require.NoError(t, f.Close())
	}
}

func TestMemWalReader(t *testing.T) {
	defer initConfig(2)()

	sid := uint64(100)
	row := buildRow(1, "foo", 10)
	row.UnmarshalIndexKeys(nil)
	rec := buildRecord(10, 88)
	lock := ""

	wal := shelf.NewWal(t.TempDir(), &lock, "foo")
	defer wal.MustClose()

	err := wal.WriteRecord(sid, row.IndexKey, rec)
	require.NoError(t, err)

	wal.LoadIntoMemory()
	require.NoError(t, wal.Sync())

	ctx, _ := shelf.NewWalCtx()
	_, err = wal.ReadBlock(ctx, 0)
	require.NoError(t, err)

	_, err = wal.ReadBlock(ctx, 10000)
	require.EqualError(t, err, io.EOF.Error())
}

func TestWalRecordCodec(t *testing.T) {
	rec := &record.Record{}
	rec.Schema = record.Schemas{
		record.Field{
			Type: 1,
			Name: "foo",
		},
		record.Field{
			Type: 1,
			Name: "foo1",
		},
		record.Field{
			Type: 1,
			Name: "time",
		},
	}
	rec.ReserveColVal(3)
	rec.ColVals[0].AppendIntegers(1)
	rec.ColVals[0].AppendIntegerNull()
	rec.ColVals[1].AppendIntegerNulls(2) // all nil
	rec.AppendTime(10, 20)

	record.CheckRecord(rec)

	codec := shelf.NewWalRecordCodec()
	buf := codec.Encode(rec, nil)

	other := &record.Record{}
	err := codec.Decode(other, slices.Clone(buf))
	require.NoError(t, err)
	require.Equal(t, rec.Schema, other.Schema)

	require.Equal(t, rec.ColVals[0].IntegerValues(), other.ColVals[0].IntegerValues())
	require.Equal(t, rec.ColVals[1], rec.ColVals[1])
	require.Equal(t, rec.Times(), other.Times())
}

func TestWalRecordCodecError(t *testing.T) {
	rec := &record.Record{}
	var buf []byte
	codec := shelf.NewWalRecordCodec()

	var decode = func(exp string) {
		err := codec.Decode(rec, buf)
		require.EqualError(t, err, exp)
	}

	decode("invalid field length")

	buf = binary.AppendUvarint(buf[:0], 1) // field length = 1
	decode("invalid field length")
	buf = append(buf, 0, 0, 4) // field type = 4

	decode(errno.NewError(errno.TooSmallOrOverflow, "ColVal.Len").Error())

	buf = binary.AppendUvarint(buf, 1) // ColVal.Len = 1
	decode(errno.NewError(errno.TooSmallOrOverflow, "ColVal.NilCount").Error())

	buf = binary.AppendUvarint(buf, 0)  // ColVal.NilCount = 0
	buf = append(buf, 0, 0, 0, 2, 1, 1) // ColVal.Val = []byte{1, 1}
	decode(errno.NewError(errno.TooSmallData, "ColVal.Offset", util.Uint32SizeBytes, 0).Error())

	buf = append(buf, 0, 0, 0, 1) // len(ColVal.Offset) = 1
	decode(errno.NewError(errno.TooSmallData, "ColVal.Offset", util.Uint32SizeBytes, 0).Error())
}
