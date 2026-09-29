package faststart_test

import (
	"context"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/faststart"
	"github.com/skylab-kulubu/core-backend/internal/faststart/mp4test"
)

// rewrite plans the rewrite of file and returns the rewritten file whole.
func rewrite(t *testing.T, file []byte) ([]byte, faststart.Layout) {
	t.Helper()
	layout, err := faststart.Plan(context.Background(), mp4test.Memory(file), int64(len(file)), faststart.DefaultLimits)
	if err != nil {
		t.Fatal(err)
	}
	out, err := layout.Read(context.Background(), mp4test.Memory(file), 0, layout.Size)
	if err != nil {
		t.Fatal(err)
	}
	return out, layout
}
