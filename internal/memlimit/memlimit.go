// Package memlimit gives the Go runtime a soft memory limit (GOMEMLIMIT)
// below the container's memory limit, so the collector works harder as
// core nears the limit instead of letting the heap grow to twice what is
// live and the kernel killing the container (docs/memory-limit.md).
package memlimit

import (
	"bytes"
	"fmt"
	"math"
	"runtime/metrics"
	"strconv"
	"strings"
)

// containerShare is the part of the container's memory limit the Go
// runtime aims to stay under; the rest is headroom for what the soft limit
// does not see (the page cache charged to the container, the kernel's
// buffers for core's sockets) and for the collector to catch up.
const containerShare = 0.9

// unlimitedCgroupV1 is above any real limit: cgroup v1 reports "no limit"
// as a huge page-rounded number.
const unlimitedCgroupV1 = 1 << 62

// Source is where the soft limit came from.
type Source string

const (
	// SourceEnv: GOMEMLIMIT was set; the runtime read it before main.
	SourceEnv Source = "env"
	// SourceContainer: containerShare of the container's memory limit.
	SourceContainer Source = "container"
	// SourceNone: no GOMEMLIMIT and no container limit; none is set.
	SourceNone Source = "none"
)

// Setting is the soft memory limit core runs with.
type Setting struct {
	// Limit is the runtime's soft limit in bytes; math.MaxInt64 when none.
	Limit int64
	// Container is the container's memory limit in bytes; 0 when none.
	Container int64
	Source    Source
}

// Apply sets the runtime's soft memory limit to 90 % of the container's
// memory limit, unless GOMEMLIMIT is set (the runtime applied it already)
// or the container has none. readFile reads the cgroup files; setLimit is
// debug.SetMemoryLimit.
func Apply(getenv func(string) string, readFile func(string) ([]byte, error), setLimit func(int64) int64) Setting {
	setting := Setting{Container: containerLimit(readFile), Source: SourceNone}
	switch {
	case strings.TrimSpace(getenv("GOMEMLIMIT")) != "":
		setting.Source = SourceEnv
	case setting.Container > 0:
		setting.Source = SourceContainer
		setLimit(int64(float64(setting.Container) * containerShare))
	}
	setting.Limit = setLimit(-1)
	return setting
}

// containerLimit is the memory limit of core's cgroup: v2's memory.max at
// the path /proc/self/cgroup names (the root in a container with its own
// cgroup namespace, Docker's default), else at the root, else v1's
// memory.limit_in_bytes. 0 when there is none or none can be read.
func containerLimit(readFile func(string) ([]byte, error)) int64 {
	var candidates []string
	if own, err := readFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(own), "\n") {
			if path, ok := strings.CutPrefix(line, "0::"); ok && path != "/" && path != "" {
				candidates = append(candidates, "/sys/fs/cgroup"+path+"/memory.max")
			}
		}
	}
	candidates = append(candidates, "/sys/fs/cgroup/memory.max", "/sys/fs/cgroup/memory/memory.limit_in_bytes")
	for _, path := range candidates {
		raw, err := readFile(path)
		if err != nil {
			continue
		}
		value := string(bytes.TrimSpace(raw))
		if value == "max" {
			return 0
		}
		limit, err := strconv.ParseInt(value, 10, 64)
		if err != nil || limit <= 0 || limit >= unlimitedCgroupV1 {
			return 0
		}
		return limit
	}
	return 0
}

func mib(n int64) int64 { return n >> 20 }

// String describes the setting for the startup log.
func (s Setting) String() string {
	switch {
	case s.Source == SourceContainer:
		return fmt.Sprintf("GOMEMLIMIT %d MiB, %d%% of the container's memory limit (%d MiB)", mib(s.Limit), int(containerShare*100), mib(s.Container))
	case s.Source == SourceEnv && s.Container > 0:
		return fmt.Sprintf("GOMEMLIMIT %s, set by GOMEMLIMIT (container memory limit %d MiB)", limitText(s.Limit), mib(s.Container))
	case s.Source == SourceEnv:
		return fmt.Sprintf("GOMEMLIMIT %s, set by GOMEMLIMIT (no container memory limit)", limitText(s.Limit))
	}
	return "no GOMEMLIMIT: the container has no memory limit and GOMEMLIMIT is unset"
}

func limitText(limit int64) string {
	if limit == math.MaxInt64 {
		return "off"
	}
	return fmt.Sprintf("%d MiB", mib(limit))
}

// Prometheus is the soft limit, the container's limit (each only when
// there is one) and the memory the Go runtime holds now, in Prometheus'
// text format.
func (s Setting) Prometheus() string {
	var out strings.Builder
	if s.Limit != math.MaxInt64 {
		fmt.Fprintf(&out, "# TYPE skylab_core_go_memory_limit_bytes gauge\nskylab_core_go_memory_limit_bytes %d\n", s.Limit)
	}
	if s.Container > 0 {
		fmt.Fprintf(&out, "# TYPE skylab_core_container_memory_limit_bytes gauge\nskylab_core_container_memory_limit_bytes %d\n", s.Container)
	}
	sample := []metrics.Sample{{Name: "/memory/classes/total:bytes"}}
	metrics.Read(sample)
	if sample[0].Value.Kind() == metrics.KindUint64 {
		fmt.Fprintf(&out, "# TYPE skylab_core_go_memory_bytes gauge\nskylab_core_go_memory_bytes %d\n", sample[0].Value.Uint64())
	}
	return out.String()
}
