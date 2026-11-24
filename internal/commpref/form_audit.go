package commpref

import (
	"context"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// PostgresFormAudit keeps FormAudit rows in form_audience_audits.
type PostgresFormAudit struct {
	pool *pgxpool.Pool
}

// NewPostgresFormAudit keeps the rows in pool.
func NewPostgresFormAudit(pool *pgxpool.Pool) *PostgresFormAudit {
	return &PostgresFormAudit{pool: pool}
}

func (p *PostgresFormAudit) RecordFormAudience(ctx context.Context, a FormAudit) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO form_audience_audits (resolved_at, actor_sub, form_id, purpose, category, promotional, recipients, client)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		a.At, a.ActorSub, a.FormID, string(a.Purpose), a.Category, a.Promotional, a.Recipients, a.Client)
	return err
}

// MemoryFormAudit keeps FormAudit rows in memory (tests, the memory
// server).
type MemoryFormAudit struct {
	mu   sync.Mutex
	rows []FormAudit
}

func (m *MemoryFormAudit) RecordFormAudience(_ context.Context, a FormAudit) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, a)
	return nil
}

// Rows are the rows kept so far.
func (m *MemoryFormAudit) Rows() []FormAudit {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]FormAudit(nil), m.rows...)
}

var (
	_ FormAuditLog = (*PostgresFormAudit)(nil)
	_ FormAuditLog = (*MemoryFormAudit)(nil)
)
