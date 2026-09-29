package media

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// A pass that panics outside a video's step (a bug) is logged by Run's
// pass, without what the panic held, and the next pass comes as usual.
func TestFramePassThatPanicsIsLoggedAndTheNextComes(t *testing.T) {
	t.Parallel()
	w := &FrameWorker{} // no store: the pass panics
	var logs []string
	next, down := w.runPass(context.Background(), func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}, 0)
	if next != framePollInterval || down != 0 {
		t.Fatalf("after a panic: next pass in %s, down wait %s", next, down)
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "a pass panicked") {
		t.Fatalf("logs %q", logs)
	}
}
