package accessgate

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

type Decision string

const (
	Allowed     Decision = "allowed"
	Blocked     Decision = "blocked"
	Unavailable Decision = "unavailable"
)

type Reader interface {
	Check(context.Context, string) Decision
	Ready(context.Context) error
}

type BlockWriter interface {
	EnsureBlocked(context.Context, string) error
}

type ProjectionWriter interface {
	BlockWriter
	AssertMarker(context.Context, string) error
	EnsureContract(context.Context) error
}

type RedisGate struct {
	client      *redis.Client
	deadline    time.Duration
	replicas    int
	waitTimeout time.Duration
	metrics     *Metrics
}

func NewRedisGate(client *redis.Client, deadline time.Duration, replicas int, waitTimeout time.Duration, metricSets ...*Metrics) *RedisGate {
	if deadline <= 0 {
		deadline = 200 * time.Millisecond
	}
	if replicas < 0 {
		replicas = 0
	}
	if waitTimeout < 0 {
		waitTimeout = 0
	}
	var metrics *Metrics
	if len(metricSets) > 0 {
		metrics = metricSets[0]
	}
	return &RedisGate{client: client, deadline: deadline, replicas: replicas, waitTimeout: waitTimeout, metrics: metrics}
}

func (g *RedisGate) Check(ctx context.Context, subject string) Decision {
	if g == nil || g.client == nil || subject == "" {
		if g != nil {
			g.metrics.RecordRedisUnavailable()
		}
		return Unavailable
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()
	values, err := g.client.MGet(opCtx, ContractKey, MarkerKey(subject)).Result()
	if err != nil || len(values) != 2 {
		g.metrics.RecordRedisUnavailable()
		return Unavailable
	}
	contract, ok := values[0].(string)
	if !ok || contract != ContractValue {
		g.metrics.RecordContractMismatch()
		return Unavailable
	}
	if values[1] == nil {
		return Allowed
	}
	marker, ok := values[1].(string)
	if !ok || marker != MarkerValue {
		g.metrics.RecordMarkerMalformed()
		return Unavailable
	}
	return Blocked
}

func (g *RedisGate) Ready(ctx context.Context) error {
	if g == nil || g.client == nil {
		if g != nil {
			g.metrics.RecordRedisUnavailable()
		}
		return errors.New("account access gate unavailable")
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()
	value, err := g.client.Get(opCtx, ContractKey).Result()
	if err != nil {
		if errors.Is(err, redis.Nil) {
			g.metrics.RecordContractMismatch()
		} else {
			g.metrics.RecordRedisUnavailable()
		}
		return errors.New("account access gate unavailable")
	}
	if value != ContractValue {
		g.metrics.RecordContractMismatch()
		return errors.New("account access gate contract mismatch")
	}
	return nil
}

func (g *RedisGate) EnsureBlocked(ctx context.Context, subject string) error {
	if err := g.AssertMarker(ctx, subject); err != nil {
		return err
	}
	return g.Ready(ctx)
}

func (g *RedisGate) AssertMarker(ctx context.Context, subject string) error {
	if g == nil || g.client == nil || subject == "" {
		if g != nil {
			g.metrics.RecordRedisUnavailable()
		}
		return errors.New("account access marker unavailable")
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()
	key := MarkerKey(subject)
	conn := g.client.Conn()
	defer conn.Close()
	if err := conn.Set(opCtx, key, MarkerValue, 0).Err(); err != nil {
		g.metrics.RecordRedisUnavailable()
		return errors.New("account access marker write failed")
	}
	if g.replicas > 0 {
		acknowledged, err := conn.Wait(opCtx, g.replicas, g.waitTimeout).Result()
		if err != nil || int(acknowledged) < g.replicas {
			g.metrics.RecordRedisUnavailable()
			return errors.New("account access marker durability acknowledgement failed")
		}
	}
	pipe := conn.Pipeline()
	marker := pipe.Get(opCtx, key)
	ttl := pipe.PTTL(opCtx, key)
	if _, err := pipe.Exec(opCtx); err != nil {
		g.metrics.RecordRedisUnavailable()
		return errors.New("account access marker verification failed")
	}
	if value, err := marker.Result(); err != nil || value != MarkerValue {
		g.metrics.RecordMarkerMalformed()
		return errors.New("account access marker verification failed")
	}
	if value, err := ttl.Result(); err != nil || value != -1 {
		if err != nil {
			g.metrics.RecordRedisUnavailable()
		} else if value > 0 {
			g.metrics.RecordPositiveTTL()
		} else {
			g.metrics.RecordMarkerMalformed()
		}
		return fmt.Errorf("account access marker is not permanent")
	}
	return nil
}

// ReconcileMarker inspects the existing projection before repairing it, so
// missing, malformed or expiring markers become aggregate alert signals.
func (g *RedisGate) ReconcileMarker(ctx context.Context, subject string) error {
	if g == nil || g.client == nil || subject == "" {
		if g != nil {
			g.metrics.RecordRedisUnavailable()
		}
		return errors.New("account access marker unavailable")
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()
	key := MarkerKey(subject)
	value, err := g.client.Get(opCtx, key).Result()
	drifted := false
	switch {
	case errors.Is(err, redis.Nil):
		drifted = true
	case err != nil:
		g.metrics.RecordRedisUnavailable()
		return errors.New("account access marker inspection failed")
	case value != MarkerValue:
		g.metrics.RecordMarkerMalformed()
		drifted = true
	}
	ttl, ttlErr := g.client.PTTL(opCtx, key).Result()
	if ttlErr != nil {
		g.metrics.RecordRedisUnavailable()
		return errors.New("account access marker inspection failed")
	}
	if ttl != -1 {
		drifted = true
		if ttl > 0 {
			g.metrics.RecordPositiveTTL()
		}
	}
	if drifted {
		g.metrics.RecordReconciliationDrift()
	}
	return g.AssertMarker(ctx, subject)
}

// EnsureContract installs the sentinel only when it is absent. A mismatched
// value is never repaired in place because namespace drift must stay fail-closed.
func (g *RedisGate) EnsureContract(ctx context.Context) error {
	if g == nil || g.client == nil {
		if g != nil {
			g.metrics.RecordRedisUnavailable()
		}
		return errors.New("account access gate unavailable")
	}
	opCtx, cancel := context.WithTimeout(ctx, g.deadline)
	defer cancel()
	conn := g.client.Conn()
	defer conn.Close()
	created, err := conn.SetNX(opCtx, ContractKey, ContractValue, 0).Result()
	if err != nil {
		g.metrics.RecordRedisUnavailable()
		return errors.New("account access contract write failed")
	}
	pipe := conn.Pipeline()
	contract := pipe.Get(opCtx, ContractKey)
	ttl := pipe.PTTL(opCtx, ContractKey)
	if _, err := pipe.Exec(opCtx); err != nil {
		g.metrics.RecordRedisUnavailable()
		return errors.New("account access contract verification failed")
	}
	if value, err := contract.Result(); err != nil || value != ContractValue {
		g.metrics.RecordContractMismatch()
		return errors.New("account access gate contract mismatch")
	}
	if value, err := ttl.Result(); err != nil || value != -1 {
		if err != nil {
			g.metrics.RecordRedisUnavailable()
		} else if value > 0 {
			g.metrics.RecordPositiveTTL()
		} else {
			g.metrics.RecordContractMismatch()
		}
		return errors.New("account access contract is not permanent")
	}
	if g.replicas > 0 {
		// SETNX may have succeeded on an earlier attempt whose WAIT failed. Once
		// the existing value and TTL are proven exact, repeat the same permanent
		// write so this connection has an offset that WAIT can acknowledge.
		if !created {
			if err := conn.Set(opCtx, ContractKey, ContractValue, 0).Err(); err != nil {
				g.metrics.RecordRedisUnavailable()
				return errors.New("account access contract write failed")
			}
		}
		acknowledged, err := conn.Wait(opCtx, g.replicas, g.waitTimeout).Result()
		if err != nil || int(acknowledged) < g.replicas {
			g.metrics.RecordRedisUnavailable()
			return errors.New("account access contract durability acknowledgement failed")
		}
	}
	return nil
}
