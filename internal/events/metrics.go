package events

import (
	"fmt"
	"slices"
	"strings"
	"sync"
)

// metrics are the consumer's counters, by queue. They name no event and no
// person.
type metrics struct {
	mu              sync.Mutex
	queues          []string
	connected       bool
	connectFailures int64
	consuming       map[string]bool
	consumedTotal   map[[2]string]int64
	redelivers      map[string]int64
	retries         map[string]int64
	dlq             map[[2]string]int64
}

func newMetrics() *metrics {
	return &metrics{
		consuming:     map[string]bool{},
		consumedTotal: map[[2]string]int64{},
		redelivers:    map[string]int64{},
		retries:       map[string]int64{},
		dlq:           map[[2]string]int64{},
	}
}

func (m *metrics) addQueue(queue string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queues = append(m.queues, queue)
	slices.Sort(m.queues)
}

func (m *metrics) setConnected(on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connected = on
}

func (m *metrics) connectFailed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.connectFailures++
}

func (m *metrics) setConsuming(queue string, on bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consuming[queue] = on
}

func (m *metrics) consumed(queue, outcome string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consumedTotal[[2]string{queue, outcome}]++
}

func (m *metrics) deadLettered(queue, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.consumedTotal[[2]string{queue, OutcomeDeadLettered}]++
	m.dlq[[2]string{queue, reason}]++
}

func (m *metrics) redelivered(queue string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.redelivers[queue]++
}

func (m *metrics) retried(queue string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retries[queue]++
}

// Prometheus is the consumer's state and counters in Prometheus' text
// format, every subscribed queue's series present from the start. A nil
// consumer (EVENTS_ENABLED off) reports enabled 0 and nothing else.
func (c *Consumer) Prometheus() string {
	var out strings.Builder
	if c == nil {
		out.WriteString("# TYPE skylab_events_enabled gauge\nskylab_events_enabled 0\n")
		return out.String()
	}
	m := c.metrics
	m.mu.Lock()
	defer m.mu.Unlock()
	out.WriteString("# TYPE skylab_events_enabled gauge\nskylab_events_enabled 1\n")
	fmt.Fprintf(&out, "# TYPE skylab_events_connected gauge\nskylab_events_connected %d\n", boolInt(m.connected))
	fmt.Fprintf(&out, "# TYPE skylab_events_connect_failures_total counter\nskylab_events_connect_failures_total %d\n", m.connectFailures)
	out.WriteString("# TYPE skylab_events_consuming gauge\n")
	for _, q := range m.queues {
		fmt.Fprintf(&out, "skylab_events_consuming{queue=%q} %d\n", q, boolInt(m.consuming[q]))
	}
	out.WriteString("# TYPE skylab_events_consumed_total counter\n")
	for _, q := range m.queues {
		for _, o := range outcomes {
			fmt.Fprintf(&out, "skylab_events_consumed_total{queue=%q,outcome=%q} %d\n", q, o, m.consumedTotal[[2]string{q, o}])
		}
	}
	out.WriteString("# TYPE skylab_events_redelivered_total counter\n")
	for _, q := range m.queues {
		fmt.Fprintf(&out, "skylab_events_redelivered_total{queue=%q} %d\n", q, m.redelivers[q])
	}
	out.WriteString("# TYPE skylab_events_retries_total counter\n")
	for _, q := range m.queues {
		fmt.Fprintf(&out, "skylab_events_retries_total{queue=%q} %d\n", q, m.retries[q])
	}
	out.WriteString("# TYPE skylab_events_dlq_total counter\n")
	for _, q := range m.queues {
		for _, r := range reasons {
			fmt.Fprintf(&out, "skylab_events_dlq_total{queue=%q,reason=%q} %d\n", q, r, m.dlq[[2]string{q, r}])
		}
	}
	return out.String()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
