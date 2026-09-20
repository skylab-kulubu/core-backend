package accessgate_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/skylab-kulubu/core-backend/internal/accessgate"
	"github.com/skylab-kulubu/core-backend/internal/testredis"
)

func TestRedisReaderFailsClosedUnlessTheExactContractAllowsTheSubject(t *testing.T) {
	client := testredis.Start(t)
	gate := accessgate.NewRedisGate(client, 200*time.Millisecond, 0, 0)
	ctx := context.Background()
	const subject = "reader-subject"

	if got := gate.Check(ctx, subject); got != accessgate.Unavailable {
		t.Fatalf("missing contract decision = %q", got)
	}
	if err := client.Set(ctx, accessgate.ContractKey, "wrong", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got := gate.Check(ctx, subject); got != accessgate.Unavailable {
		t.Fatalf("wrong contract decision = %q", got)
	}
	if err := client.Set(ctx, accessgate.ContractKey, accessgate.ContractValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got := gate.Check(ctx, subject); got != accessgate.Allowed {
		t.Fatalf("unmarked decision = %q", got)
	}
	if err := client.Set(ctx, accessgate.MarkerKey(subject), "unexpected", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got := gate.Check(ctx, subject); got != accessgate.Unavailable {
		t.Fatalf("malformed marker decision = %q", got)
	}
	if err := client.Set(ctx, accessgate.MarkerKey(subject), accessgate.MarkerValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got := gate.Check(ctx, subject); got != accessgate.Blocked {
		t.Fatalf("blocked decision = %q", got)
	}
}

func TestRedisWriterReplacesTTLAndVerifiesPermanentMarker(t *testing.T) {
	client := testredis.Start(t)
	gate := accessgate.NewRedisGate(client, 200*time.Millisecond, 0, 0)
	ctx := context.Background()
	const subject = "writer-subject"

	if err := client.Set(ctx, accessgate.ContractKey, accessgate.ContractValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, accessgate.MarkerKey(subject), accessgate.MarkerValue, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := gate.EnsureBlocked(ctx, subject); err != nil {
		t.Fatal(err)
	}
	if ttl, err := client.PTTL(ctx, accessgate.MarkerKey(subject)).Result(); err != nil || ttl != -1 {
		t.Fatalf("marker TTL = %s, err=%v", ttl, err)
	}
	if got := gate.Check(ctx, subject); got != accessgate.Blocked {
		t.Fatalf("decision after write = %q", got)
	}
}

func TestRedisContractInstallerRejectsAnExistingExpiringSentinel(t *testing.T) {
	client := testredis.Start(t)
	gate := accessgate.NewRedisGate(client, 200*time.Millisecond, 0, 0)
	ctx := context.Background()
	if err := client.Set(ctx, accessgate.ContractKey, accessgate.ContractValue, time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := gate.EnsureContract(ctx); err == nil {
		t.Fatal("expiring contract sentinel was accepted")
	}
}

func TestRedisWriterRequiresConfiguredReplicaAcknowledgementOnEveryRetry(t *testing.T) {
	client := testredis.Start(t)
	gate := accessgate.NewRedisGate(client, time.Second, 1, 50*time.Millisecond)
	ctx := context.Background()
	if err := client.Set(ctx, accessgate.ContractKey, accessgate.ContractValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := gate.EnsureBlocked(ctx, "replica-required-subject"); err == nil {
			t.Fatalf("marker retry %d succeeded without a replica acknowledgement", attempt+1)
		}
	}
	if err := client.Del(ctx, accessgate.ContractKey).Err(); err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := gate.EnsureContract(ctx); err == nil {
			t.Fatalf("contract retry %d succeeded without a replica acknowledgement", attempt+1)
		}
	}
}

func TestRedisReaderFailsClosedAfterConnectionLoss(t *testing.T) {
	client := testredis.Start(t)
	gate := accessgate.NewRedisGate(client, 50*time.Millisecond, 0, 0)
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if got := gate.Check(context.Background(), "connection-loss-subject"); got != accessgate.Unavailable {
		t.Fatalf("decision = %q", got)
	}
	if err := gate.Ready(context.Background()); err == nil {
		t.Fatal("readiness passed after connection loss")
	}
}

func TestRedisMetricsSurfaceContractMarkerAndReconciliationFailures(t *testing.T) {
	client := testredis.Start(t)
	metrics := accessgate.NewMetrics()
	gate := accessgate.NewRedisGate(client, 200*time.Millisecond, 0, 0, metrics)
	ctx := context.Background()
	const subject = "metrics-reason-subject"

	if got := gate.Check(ctx, subject); got != accessgate.Unavailable {
		t.Fatalf("missing contract decision = %q", got)
	}
	if err := client.Set(ctx, accessgate.ContractKey, accessgate.ContractValue, 0).Err(); err != nil {
		t.Fatal(err)
	}
	if err := client.Set(ctx, accessgate.MarkerKey(subject), "unexpected", 0).Err(); err != nil {
		t.Fatal(err)
	}
	if got := gate.Check(ctx, subject); got != accessgate.Unavailable {
		t.Fatalf("malformed marker decision = %q", got)
	}
	if err := client.Set(ctx, accessgate.MarkerKey(subject), "unexpected", time.Minute).Err(); err != nil {
		t.Fatal(err)
	}
	if err := gate.ReconcileMarker(ctx, subject); err != nil {
		t.Fatal(err)
	}

	rendered := metrics.Prometheus()
	for _, line := range []string{
		"skylab_account_access_contract_mismatch_total 1\n",
		"skylab_account_access_marker_malformed_total 2\n",
		"skylab_account_access_reconciliation_drift_total 1\n",
		"skylab_account_access_positive_ttl_total 1\n",
	} {
		if !strings.Contains(rendered, line) {
			t.Fatalf("metrics missing %q:\n%s", line, rendered)
		}
	}
}
