package mergeset

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/VictoriaMetrics/VictoriaMetrics/lib/logger"
	"github.com/openGemini/openGemini/lib/fileops"
	"github.com/stretchr/testify/assert"
)

func TestTableOpenClose(t *testing.T) {
	const path = "TestTableOpenClose"
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("cannot remove %q: %s", path, err)
	}
	defer func() {
		_ = os.RemoveAll(path)
	}()
	lockPath := ""
	// Create a new table
	tb, err := OpenTable(path, nil, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot create new table: %s", err)
	}

	// Close it
	tb.MustClose()

	// Re-open created table multiple times.
	for i := 0; i < 10; i++ {
		tb, err := OpenTable(path, nil, nil, &lockPath)
		if err != nil {
			t.Fatalf("cannot open created table: %s", err)
		}
		tb.MustClose()
	}
}

func TestTableAddItemSerial(t *testing.T) {
	const path = "TestTableAddItemSerial"
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("cannot remove %q: %s", path, err)
	}
	defer func() {
		_ = os.RemoveAll(path)
	}()
	lockPath := ""
	var flushes uint64
	flushCallback := func() {
		atomic.AddUint64(&flushes, 1)
	}
	tb, err := OpenTable(path, flushCallback, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}

	const itemsCount = 1e5
	testAddItemsSerial(tb, itemsCount)

	// Verify items count after pending items flush.
	tb.DebugFlush()
	if atomic.LoadUint64(&flushes) == 0 {
		t.Fatalf("unexpected zero flushes")
	}

	var m TableMetrics
	tb.UpdateMetrics(&m)
	if m.ItemsCount != itemsCount {
		t.Fatalf("unexpected itemsCount; got %d; want %v", m.ItemsCount, itemsCount)
	}

	tb.MustClose()

	// Re-open the table and make sure ItemsCount remains the same.
	testReopenTable(t, path, itemsCount)
	// Add more items in order to verify merge between inmemory parts and file-based parts.
	tb, err = OpenTable(path, nil, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}
	const moreItemsCount = itemsCount * 3
	testAddItemsSerial(tb, moreItemsCount)
	tb.MustClose()

	// Re-open the table and verify ItemsCount again.
	testReopenTable(t, path, itemsCount+moreItemsCount)
}

func testAddItemsSerial(tb *Table, itemsCount int) {
	for i := 0; i < itemsCount; i++ {
		item := getRandomBytes()
		if len(item) > maxInmemoryBlockSize {
			item = item[:maxInmemoryBlockSize]
		}
		if err := tb.AddItems([][]byte{item}); err != nil {
			logger.Panicf("BUG: cannot add item to table: %s", err)
		}
	}
}

func TestTableCreateSnapshotAt(t *testing.T) {
	const path = "TestTableCreateSnapshotAt"
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("cannot remove %q: %s", path, err)
	}
	defer func() {
		_ = os.RemoveAll(path)
	}()
	lockPath := ""
	tb, err := OpenTable(path, nil, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}
	defer tb.MustClose()

	// Write a lot of items into the table, so background merges would start.
	const itemsCount = 3e5
	for i := 0; i < itemsCount; i++ {
		item := []byte(fmt.Sprintf("item %d", i))
		if err := tb.AddItems([][]byte{item}); err != nil {
			t.Fatalf("cannot add item to table: %s", err)
		}
	}
	tb.DebugFlush()

	// Create multiple snapshots.
	snapshot1 := path + "-test-snapshot1"
	if err := tb.CreateSnapshotAt(snapshot1, &lockPath); err != nil {
		t.Fatalf("cannot create snapshot1: %s", err)
	}
	snapshot2 := path + "-test-snapshot2"
	if err := tb.CreateSnapshotAt(snapshot2, &lockPath); err != nil {
		t.Fatalf("cannot create snapshot2: %s", err)
	}
	defer func() {
		_ = os.RemoveAll(snapshot1)
		_ = os.RemoveAll(snapshot2)
	}()

	// Verify snapshots contain all the data.
	tb1, err := OpenTable(snapshot1, nil, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}
	defer tb1.MustClose()

	tb2, err := OpenTable(snapshot2, nil, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}
	defer tb2.MustClose()

	var ts, ts1, ts2 TableSearch
	ts.Init(tb)
	ts1.Init(tb1)
	defer ts1.MustClose()
	ts2.Init(tb2)
	defer ts2.MustClose()
	for i := 0; i < itemsCount; i++ {
		key := []byte(fmt.Sprintf("item %d", i))
		if err := ts.FirstItemWithPrefix(key); err != nil {
			t.Fatalf("cannot find item[%d]=%q in the original table: %s", i, key, err)
		}
		if !bytes.Equal(key, ts.Item) {
			t.Fatalf("unexpected item found for key=%q in the original table; got %q", key, ts.Item)
		}
		if err := ts1.FirstItemWithPrefix(key); err != nil {
			t.Fatalf("cannot find item[%d]=%q in snapshot1: %s", i, key, err)
		}
		if !bytes.Equal(key, ts1.Item) {
			t.Fatalf("unexpected item found for key=%q in snapshot1; got %q", key, ts1.Item)
		}
		if err := ts2.FirstItemWithPrefix(key); err != nil {
			t.Fatalf("cannot find item[%d]=%q in snapshot2: %s", i, key, err)
		}
		if !bytes.Equal(key, ts2.Item) {
			t.Fatalf("unexpected item found for key=%q in snapshot2; got %q", key, ts2.Item)
		}
	}
}

func TestTableAddItemsConcurrent(t *testing.T) {
	const path = "TestTableAddItemsConcurrent"
	if err := os.RemoveAll(path); err != nil {
		t.Fatalf("cannot remove %q: %s", path, err)
	}
	defer func() {
		_ = os.RemoveAll(path)
	}()
	lockPath := ""
	var flushes uint64
	flushCallback := func() {
		atomic.AddUint64(&flushes, 1)
	}
	var itemsMerged uint64
	prepareBlock := func(data []byte, items []Item) ([]byte, []Item) {
		atomic.AddUint64(&itemsMerged, uint64(len(items)))
		return data, items
	}
	tb, err := OpenTable(path, flushCallback, prepareBlock, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}

	const itemsCount = 1e5
	testAddItemsConcurrent(tb, itemsCount)

	// Verify items count after pending items flush.
	tb.DebugFlush()
	if atomic.LoadUint64(&flushes) == 0 {
		t.Fatalf("unexpected zero flushes")
	}
	n := atomic.LoadUint64(&itemsMerged)
	if n < itemsCount {
		t.Fatalf("too low number of items merged; got %v; must be at least %v", n, itemsCount)
	}

	var m TableMetrics
	tb.UpdateMetrics(&m)
	if m.ItemsCount != itemsCount {
		t.Fatalf("unexpected itemsCount; got %d; want %v", m.ItemsCount, itemsCount)
	}

	tb.MustClose()

	// Re-open the table and make sure ItemsCount remains the same.
	testReopenTable(t, path, itemsCount)

	// Add more items in order to verify merge between inmemory parts and file-based parts.
	tb, err = OpenTable(path, nil, nil, &lockPath)
	if err != nil {
		t.Fatalf("cannot open %q: %s", path, err)
	}
	const moreItemsCount = itemsCount * 3
	testAddItemsConcurrent(tb, moreItemsCount)
	tb.MustClose()

	// Re-open the table and verify ItemsCount again.
	testReopenTable(t, path, itemsCount+moreItemsCount)
}

func testAddItemsConcurrent(tb *Table, itemsCount int) {
	const goroutinesCount = 6
	workCh := make(chan int, itemsCount)
	var wg sync.WaitGroup
	for i := 0; i < goroutinesCount; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range workCh {
				item := getRandomBytes()
				if len(item) > maxInmemoryBlockSize {
					item = item[:maxInmemoryBlockSize]
				}
				if err := tb.AddItems([][]byte{item}); err != nil {
					logger.Panicf("BUG: cannot add item to table: %s", err)
				}
			}
		}()
	}
	for i := 0; i < itemsCount; i++ {
		workCh <- i
	}
	close(workCh)
	wg.Wait()
}

func testReopenTable(t *testing.T, path string, itemsCount int) {
	t.Helper()
	lockPath := ""
	for i := 0; i < 10; i++ {
		tb, err := OpenTable(path, nil, nil, &lockPath)
		if err != nil {
			t.Fatalf("cannot re-open %q: %s", path, err)
		}
		var m TableMetrics
		tb.UpdateMetrics(&m)
		if m.ItemsCount != uint64(itemsCount) {
			t.Fatalf("unexpected itemsCount after re-opening; got %d; want %v", m.ItemsCount, itemsCount)
		}
		tb.MustClose()
	}
}

func TestCheckBloomFilterFiles(t *testing.T) {
	dir := t.TempDir()
	_ = fileops.RemoveAll(dir)

	if err := checkBloomFilterFiles("/nonexistent/directory"); err == nil {
		t.Error("Expected error, got nil")
	}
	err := os.Mkdir(dir, 0750)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "file.unsupported"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkBloomFilterFiles(dir); err == nil {
		t.Error("Expected error, got nil")
	}

	err = os.RemoveAll(filepath.Join(dir, "file.unsupported"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "file"+TmpBloomFilterFileSuffix), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkBloomFilterFiles(dir); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}

	if err := os.WriteFile(filepath.Join(dir, "file"+LastBloomFilterFileSuffix), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkBloomFilterFiles(dir); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
	err = os.RemoveAll(filepath.Join(dir, "file"))
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "file"+BloomFilterFileSuffix), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := checkBloomFilterFiles(dir); err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
}

func TestRenameOldFile(t *testing.T) {
	dir := t.TempDir()
	_ = fileops.RemoveAll(dir)

	exist, err := renameOldFile("/nonexistent/directory")
	if err != nil {
		t.Fatal(err)
	}

	assert.Equal(t, false, exist)
	err = os.Mkdir(dir, 0750)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(dir, "file.bf"), []byte("content"), 0644); err != nil {
		t.Fatal(err)
	}

	exist, err = renameOldFile(filepath.Join(dir, "file.bf"))
	if err != nil {
		t.Fatal(err)
	}
	assert.Equal(t, true, exist)
}
