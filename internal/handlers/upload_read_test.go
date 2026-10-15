package handlers

import (
	"bytes"
	"runtime"
	"testing"
)

func TestReadUploadedFileHoldsOneCopy(t *testing.T) {
	content := bytes.Repeat([]byte{7}, 8<<20)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	data, err := readUploadedFile(bytes.NewReader(content), int64(len(content)))
	runtime.ReadMemStats(&after)
	if err != nil || !bytes.Equal(data, content) {
		t.Fatalf("read %d bytes, err %v", len(data), err)
	}
	// io.ReadAll would grow its buffer step by step, allocating several
	// times the file and holding two copies at its last step.
	if grew := after.TotalAlloc - before.TotalAlloc; grew > uint64(len(content))+1<<20 {
		t.Fatalf("allocated %d MiB to read %d MiB", grew>>20, len(content)>>20)
	}
}

func TestReadUploadedFileRefusesASizeThatIsNotTheFiles(t *testing.T) {
	t.Parallel()
	if _, err := readUploadedFile(bytes.NewReader([]byte("abc")), 4); err == nil {
		t.Fatal("a file shorter than its size was read")
	}
	if _, err := readUploadedFile(bytes.NewReader([]byte("abcd")), 3); err == nil {
		t.Fatal("a file longer than its size was cut short")
	}
	if data, err := readUploadedFile(bytes.NewReader(nil), 0); err != nil || len(data) != 0 {
		t.Fatalf("empty file: %q, %v", data, err)
	}
}
