package formsfeed

import (
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// knownOutcomes are the answer outcomes /v1/metrics always lists for each
// way in; another the Ticket rule may add shows once it happens.
var knownOutcomes = []string{
	string(ticket.FormResponseRecorded), string(ticket.FormResponseNotAccepted), string(ticket.FormResponseNoRespondent),
	string(ticket.FormResponseNotListed), string(ticket.FormResponsePersonUnavailable), string(ticket.FormResponseEventCancelled),
	string(ticket.FormResponseRegistrationClosed), string(ticket.FormResponseEventEnded), string(ticket.FormResponseFeedback),
	string(ticket.FormResponseWaitlisted), string(ticket.FormResponseEventFull),
	OutcomeStale, OutcomeGone, OutcomeInvalid, OutcomeFailed,
}

var (
	knownVias       = []string{ViaEvent, ViaPull}
	knownReads      = []string{"ok", "not_found", "forbidden", "invalid", "failed", "off"}
	knownFormStates = []string{"processed", OutcomeStale, OutcomeFailed}
)

type metrics struct {
	mu         sync.Mutex
	answers    map[[2]string]int64
	reads      map[string]int64
	formStates map[string]int64
}

func newMetrics() *metrics {
	m := &metrics{answers: map[[2]string]int64{}, reads: map[string]int64{}, formStates: map[string]int64{}}
	for _, via := range knownVias {
		for _, outcome := range knownOutcomes {
			m.answers[[2]string{via, outcome}] = 0
		}
	}
	for _, r := range knownReads {
		m.reads[r] = 0
	}
	for _, s := range knownFormStates {
		m.formStates[s] = 0
	}
	return m
}

func (m *metrics) answer(via, outcome string) {
	m.mu.Lock()
	m.answers[[2]string{via, outcome}]++
	m.mu.Unlock()
}

func (m *metrics) read(result string) {
	m.mu.Lock()
	m.reads[result]++
	m.mu.Unlock()
}

func (m *metrics) formState(outcome string) {
	m.mu.Lock()
	m.formStates[outcome]++
	m.mu.Unlock()
}

func (m *metrics) prometheus() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	b.WriteString("# TYPE skylab_forms_answers_total counter\n")
	keys := make([][2]string, 0, len(m.answers))
	for k := range m.answers {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	for _, k := range keys {
		fmt.Fprintf(&b, "skylab_forms_answers_total{via=%q,outcome=%q} %d\n", k[0], k[1], m.answers[k])
	}
	writeCounter(&b, "skylab_forms_answer_reads_total", "result", m.reads)
	writeCounter(&b, "skylab_forms_form_states_total", "outcome", m.formStates)
	return b.String()
}

func writeCounter(b *strings.Builder, name, label string, values map[string]int64) {
	fmt.Fprintf(b, "# TYPE %s counter\n", name)
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(b, "%s{%s=%q} %d\n", name, label, k, values[k])
	}
}
