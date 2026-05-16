package cloud

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

type progressCall struct {
	downloaded int64
	total      int64
}

func TestProgressReaderRead(t *testing.T) {
	tests := []struct {
		name          string
		data          string
		bufSize       int
		total         int64
		expectedCalls []progressCall
	}{
		{
			name:    "single read",
			data:    "hello",
			bufSize: 5,
			total:   5,
			expectedCalls: []progressCall{
				{downloaded: 5, total: 5},
			},
		},
		{
			name:    "multiple reads",
			data:    "hello world",
			bufSize: 3,
			total:   11,
			expectedCalls: []progressCall{
				{downloaded: 3, total: 11},
				{downloaded: 6, total: 11},
				{downloaded: 9, total: 11},
				{downloaded: 11, total: 11},
			},
		},
		{
			name:          "zero total skips callback",
			data:          "hello",
			bufSize:       5,
			total:         0,
			expectedCalls: nil,
		},
		{
			name:          "empty data",
			data:          "",
			bufSize:       5,
			total:         0,
			expectedCalls: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls []progressCall
			reader := &progressReader{
				reader: bytes.NewReader([]byte(tt.data)),
				total:  tt.total,
				onProgress: func(downloaded, total int64) {
					calls = append(calls, progressCall{downloaded: downloaded, total: total})
				},
			}

			buf := make([]byte, tt.bufSize)
			totalRead := 0
			for {
				n, err := reader.Read(buf)
				totalRead += n
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Read() error: %v", err)
				}
			}

			if totalRead != len(tt.data) {
				t.Fatalf("read %d bytes, want %d", totalRead, len(tt.data))
			}
			if len(calls) != len(tt.expectedCalls) {
				t.Fatalf("got %d progress calls, want %d", len(calls), len(tt.expectedCalls))
			}
			for i, want := range tt.expectedCalls {
				if calls[i] != want {
					t.Fatalf("call %d = %+v, want %+v", i, calls[i], want)
				}
			}
		})
	}
}

func TestProgressReaderNilCallbackDoesNotPanic(t *testing.T) {
	reader := &progressReader{
		reader: bytes.NewReader([]byte("hello")),
		total:  5,
	}
	buf := make([]byte, 5)

	n, err := reader.Read(buf)
	if err != nil {
		t.Fatalf("Read() error: %v", err)
	}
	if n != 5 {
		t.Fatalf("Read() read %d bytes, want 5", n)
	}
}

func TestDriveParentQueryEscapesFolderID(t *testing.T) {
	got := driveParentQuery(`abc'\def`)
	want := `'abc\'\\def' in parents and trashed = false`
	if got != want {
		t.Fatalf("driveParentQuery() = %q, want %q", got, want)
	}
}

func TestSecureCreateLocalFileRejectsSymlinkDestination(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.txt")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}

	f, err := secureCreateLocalFile(link)
	if err == nil {
		_ = f.Close()
		t.Fatal("expected symlink destination to be rejected")
	}
}
