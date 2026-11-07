package apppref

import (
	"sort"
	"strconv"
	"strings"
	"sync"
)

// valueBuckets are the written value size histogram's upper bounds, bytes.
var valueBuckets = []int{64, 256, 1024, 2048, MaxValueBytes}

// Metrics count the routes' answers by app, operation and outcome, and the
// size of the values written. No person and no value ever reaches them. An
// app outside the client map is counted as "other", the summary as "all".
type Metrics struct {
	config   Config
	mu       sync.Mutex
	requests map[[3]string]uint64
	values   map[string]*histogram
}

type histogram struct {
	buckets []uint64
	sum     uint64
	count   uint64
}

func NewMetrics(config Config) *Metrics {
	return &Metrics{config: config, requests: map[[3]string]uint64{}, values: map[string]*histogram{}}
}

func (m *Metrics) label(app string) string {
	switch {
	case app == "":
		return "all"
	case m.config.Known(app):
		return app
	}
	return "other"
}

// Record counts one answer.
func (m *Metrics) Record(app, op, outcome string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests[[3]string{m.label(app), op, outcome}]++
}

// ObserveValue counts one written value of size bytes.
func (m *Metrics) ObserveValue(app string, size int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	label := m.label(app)
	h := m.values[label]
	if h == nil {
		h = &histogram{buckets: make([]uint64, len(valueBuckets))}
		m.values[label] = h
	}
	for i, bound := range valueBuckets {
		if size <= bound {
			h.buckets[i]++
		}
	}
	h.sum += uint64(size)
	h.count++
}

// Prometheus is the text exposition.
func (m *Metrics) Prometheus() string {
	if m == nil {
		return ""
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out strings.Builder
	out.WriteString("# TYPE skylab_app_preferences_requests_total counter\n")
	keys := make([][3]string, 0, len(m.requests))
	for k := range m.requests {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		return strings.Join(keys[i][:], "\x00") < strings.Join(keys[j][:], "\x00")
	})
	for _, k := range keys {
		out.WriteString(`skylab_app_preferences_requests_total{app="` + k[0] + `",op="` + k[1] + `",outcome="` + k[2] + `"} ` +
			strconv.FormatUint(m.requests[k], 10) + "\n")
	}
	out.WriteString("# TYPE skylab_app_preferences_value_bytes histogram\n")
	apps := make([]string, 0, len(m.values))
	for app := range m.values {
		apps = append(apps, app)
	}
	sort.Strings(apps)
	for _, app := range apps {
		h := m.values[app]
		for i, bound := range valueBuckets {
			out.WriteString(`skylab_app_preferences_value_bytes_bucket{app="` + app + `",le="` + strconv.Itoa(bound) + `"} ` +
				strconv.FormatUint(h.buckets[i], 10) + "\n")
		}
		out.WriteString(`skylab_app_preferences_value_bytes_bucket{app="` + app + `",le="+Inf"} ` + strconv.FormatUint(h.count, 10) + "\n")
		out.WriteString(`skylab_app_preferences_value_bytes_sum{app="` + app + `"} ` + strconv.FormatUint(h.sum, 10) + "\n")
		out.WriteString(`skylab_app_preferences_value_bytes_count{app="` + app + `"} ` + strconv.FormatUint(h.count, 10) + "\n")
	}
	return out.String()
}
