package memlimit_test

import (
	"io/fs"
	"math"
	"strings"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/memlimit"
)

// files is a fake file system: path to content; anything else is missing.
func files(contents map[string]string) func(string) ([]byte, error) {
	return func(path string) ([]byte, error) {
		if content, ok := contents[path]; ok {
			return []byte(content), nil
		}
		return nil, fs.ErrNotExist
	}
}

func env(values map[string]string) func(string) string {
	return func(key string) string { return values[key] }
}

// runtimeLimit records what Apply sets, starting from the runtime's
// default (no limit) or from what GOMEMLIMIT set.
type runtimeLimit struct {
	current int64
	sets    []int64
}

func (r *runtimeLimit) set(limit int64) int64 {
	previous := r.current
	if limit >= 0 {
		r.current = limit
		r.sets = append(r.sets, limit)
	}
	return previous
}

func TestApplyDerivesTheLimitFromTheContainersCgroupV2(t *testing.T) {
	t.Parallel()
	limit := &runtimeLimit{current: math.MaxInt64}
	got := memlimit.Apply(env(nil), files(map[string]string{
		"/proc/self/cgroup":         "0::/\n",
		"/sys/fs/cgroup/memory.max": "2147483648\n",
	}), limit.set)
	want := int64(2147483648) * 9 / 10
	if got.Limit != want || got.Container != 2147483648 || got.Source != memlimit.SourceContainer {
		t.Fatalf("got %+v, want limit %d from the container's 2 GiB", got, want)
	}
	if len(limit.sets) != 1 || limit.sets[0] != want {
		t.Fatalf("set %v, want [%d]", limit.sets, want)
	}
	if s := got.String(); s != "GOMEMLIMIT 1843 MiB, 90% of the container's memory limit (2048 MiB)" {
		t.Fatalf("String() = %q", s)
	}
}

func TestApplyFollowsANestedCgroupPath(t *testing.T) {
	t.Parallel()
	limit := &runtimeLimit{current: math.MaxInt64}
	got := memlimit.Apply(env(nil), files(map[string]string{
		"/proc/self/cgroup": "0::/system.slice/docker-abc.scope\n",
		"/sys/fs/cgroup/system.slice/docker-abc.scope/memory.max": "1073741824\n",
		"/sys/fs/cgroup/memory.max":                               "max\n",
	}), limit.set)
	if got.Container != 1<<30 || got.Source != memlimit.SourceContainer {
		t.Fatalf("got %+v, want the nested cgroup's 1 GiB", got)
	}
}

func TestApplyReadsCgroupV1(t *testing.T) {
	t.Parallel()
	limit := &runtimeLimit{current: math.MaxInt64}
	got := memlimit.Apply(env(nil), files(map[string]string{
		"/sys/fs/cgroup/memory/memory.limit_in_bytes": "1073741824\n",
	}), limit.set)
	if got.Container != 1<<30 || got.Limit != (1<<30)*9/10 {
		t.Fatalf("got %+v", got)
	}
}

func TestApplyLeavesNoLimitWithoutAContainerLimit(t *testing.T) {
	t.Parallel()
	for name, fsys := range map[string]map[string]string{
		"v2 max":       {"/proc/self/cgroup": "0::/\n", "/sys/fs/cgroup/memory.max": "max\n"},
		"v1 unlimited": {"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712\n"},
		"no cgroup":    {},
		"garbage":      {"/sys/fs/cgroup/memory.max": "lots\n"},
	} {
		limit := &runtimeLimit{current: math.MaxInt64}
		got := memlimit.Apply(env(nil), files(fsys), limit.set)
		if got.Source != memlimit.SourceNone || got.Container != 0 || got.Limit != math.MaxInt64 || len(limit.sets) != 0 {
			t.Errorf("%s: got %+v, sets %v; want no limit", name, got, limit.sets)
		}
		if s := got.String(); !strings.HasPrefix(s, "no GOMEMLIMIT") {
			t.Errorf("%s: String() = %q", name, s)
		}
	}
}

func TestApplyKeepsAnExplicitGOMEMLIMIT(t *testing.T) {
	t.Parallel()
	// The runtime read GOMEMLIMIT before main; Apply only reports it.
	limit := &runtimeLimit{current: 1500 << 20}
	got := memlimit.Apply(env(map[string]string{"GOMEMLIMIT": "1500MiB"}), files(map[string]string{
		"/sys/fs/cgroup/memory.max": "2147483648\n",
	}), limit.set)
	if got.Source != memlimit.SourceEnv || got.Limit != 1500<<20 || got.Container != 2147483648 || len(limit.sets) != 0 {
		t.Fatalf("got %+v, sets %v", got, limit.sets)
	}
	if s := got.String(); s != "GOMEMLIMIT 1500 MiB, set by GOMEMLIMIT (container memory limit 2048 MiB)" {
		t.Fatalf("String() = %q", s)
	}
}

func TestSettingMetrics(t *testing.T) {
	t.Parallel()
	limit := &runtimeLimit{current: math.MaxInt64}
	setting := memlimit.Apply(env(nil), files(map[string]string{"/sys/fs/cgroup/memory.max": "1073741824\n"}), limit.set)
	text := setting.Prometheus()
	for _, want := range []string{
		"# TYPE skylab_core_go_memory_limit_bytes gauge\nskylab_core_go_memory_limit_bytes 966367641\n",
		"# TYPE skylab_core_container_memory_limit_bytes gauge\nskylab_core_container_memory_limit_bytes 1073741824\n",
		"# TYPE skylab_core_go_memory_bytes gauge\nskylab_core_go_memory_bytes ",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("no %q in\n%s", want, text)
		}
	}
	none := memlimit.Apply(env(nil), files(nil), (&runtimeLimit{current: math.MaxInt64}).set).Prometheus()
	if strings.Contains(none, "limit_bytes") || !strings.Contains(none, "skylab_core_go_memory_bytes ") {
		t.Errorf("without a limit:\n%s", none)
	}
}
