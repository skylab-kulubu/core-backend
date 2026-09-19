package certificate

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

const batchCols = `id,event_id,template_version_id,template_source,reason,status,requested_by,total_count,queued_count,issued_count,failed_count,created_at,started_at,completed_at`
const jobCols = `id,batch_id,event_id,ticket_id,status,attempt_count,next_attempt_at,lease_until,error_code,certificate_id,created_at,started_at,completed_at`
const claimedJobCols = `j.id,j.batch_id,j.event_id,j.ticket_id,j.status,j.attempt_count,j.next_attempt_at,j.lease_until,j.error_code,j.certificate_id,j.created_at,j.started_at,j.completed_at`

func (s *PostgresStore) CreateBatch(ctx context.Context, b Batch, ticketIDs []uuid.UUID) (Batch, error) {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return Batch{}, err
	}
	defer tx.Rollback(ctx)
	if b.Reason == "finalization" {
		if _, err := tx.Exec(ctx, `INSERT INTO certificate_event_state (event_id,attendance_finalized_at,attendance_finalized_by) VALUES ($1,now(),$2)`, b.EventID, nullableUUIDPtr(b.RequestedBy)); err != nil {
			if isUnique(err) {
				return Batch{}, ErrConflict
			}
			return Batch{}, err
		}
	}
	b.TotalCount = len(ticketIDs)
	b.QueuedCount = len(ticketIDs)
	status := "queued"
	if len(ticketIDs) == 0 {
		status = "completed"
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO certificate_batches (id,event_id,template_version_id,template_source,reason,status,requested_by,total_count,queued_count,completed_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$8,CASE WHEN $8=0 THEN now() END) RETURNING `+batchCols,
		b.ID, b.EventID, b.TemplateVersionID, b.TemplateSource, b.Reason, status, b.RequestedBy, len(ticketIDs)).Scan(batchScan(&b)...)
	if err != nil {
		return Batch{}, err
	}
	for _, ticketID := range ticketIDs {
		if _, err := tx.Exec(ctx, `INSERT INTO certificate_jobs (id,batch_id,event_id,ticket_id) VALUES ($1,$2,$3,$4)`, uuid.New(), b.ID, b.EventID, ticketID); err != nil {
			return Batch{}, err
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return Batch{}, err
	}
	return s.GetBatch(ctx, b.ID)
}

func (s *PostgresStore) GetBatch(ctx context.Context, id uuid.UUID) (Batch, error) {
	var b Batch
	err := s.pool.QueryRow(ctx, `SELECT `+batchCols+` FROM certificate_batches WHERE id=$1`, id).Scan(batchScan(&b)...)
	if errors.Is(err, pgx.ErrNoRows) {
		return Batch{}, ErrNotFound
	}
	if err != nil {
		return Batch{}, err
	}
	rows, err := s.pool.Query(ctx, `SELECT `+jobCols+` FROM certificate_jobs WHERE batch_id=$1 ORDER BY created_at`, id)
	if err != nil {
		return Batch{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var j Job
		if err := rows.Scan(jobScan(&j)...); err != nil {
			return Batch{}, err
		}
		b.Jobs = append(b.Jobs, j)
	}
	return b, rows.Err()
}

func (s *PostgresStore) ListBatches(ctx context.Context, eventID uuid.UUID) ([]Batch, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+batchCols+` FROM certificate_batches WHERE event_id=$1 ORDER BY created_at DESC`, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Batch, 0)
	for rows.Next() {
		var b Batch
		if err := rows.Scan(batchScan(&b)...); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ClaimJobs(ctx context.Context, limit int, lease time.Duration) ([]Job, error) {
	if limit <= 0 {
		limit = 10
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	rows, err := s.pool.Query(ctx, `
		WITH picked AS (
			SELECT id FROM certificate_jobs
			WHERE ((status='queued' AND next_attempt_at<=now()) OR (status='running' AND lease_until<now()))
			ORDER BY created_at FOR UPDATE SKIP LOCKED LIMIT $1
		)
		UPDATE certificate_jobs j SET status='running',attempt_count=attempt_count+1,
			lease_until=now()+$2::interval,started_at=COALESCE(started_at,now()),error_code=''
		FROM picked WHERE j.id=picked.id RETURNING `+claimedJobCols, limit, lease.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Job, 0)
	for rows.Next() {
		var j Job
		if err := rows.Scan(jobScan(&j)...); err != nil {
			return nil, err
		}
		out = append(out, j)
	}
	for _, j := range out {
		_, _ = s.pool.Exec(ctx, `UPDATE certificate_batches SET status='running',started_at=COALESCE(started_at,now()) WHERE id=$1 AND status='queued'`, j.BatchID)
	}
	return out, rows.Err()
}

func (s *PostgresStore) CompleteJob(ctx context.Context, jobID, certificateID uuid.UUID) error {
	var batchID uuid.UUID
	err := s.pool.QueryRow(ctx, `UPDATE certificate_jobs SET status='issued',certificate_id=$2,lease_until=NULL,completed_at=now() WHERE id=$1 RETURNING batch_id`, jobID, certificateID).Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.refreshBatch(ctx, batchID)
}

func (s *PostgresStore) FailJob(ctx context.Context, jobID uuid.UUID, errorCode string, retryAt time.Time, terminal bool) error {
	status := "queued"
	completed := false
	if terminal {
		status = "failed"
		completed = true
	}
	var batchID uuid.UUID
	err := s.pool.QueryRow(ctx, `UPDATE certificate_jobs SET status=$2,error_code=$3,next_attempt_at=$4,lease_until=NULL,completed_at=CASE WHEN $5 THEN now() ELSE NULL END WHERE id=$1 RETURNING batch_id`, jobID, status, errorCode, retryAt, completed).Scan(&batchID)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return s.refreshBatch(ctx, batchID)
}

func (s *PostgresStore) RetryBatch(ctx context.Context, batchID uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE certificate_jobs SET status='queued',next_attempt_at=now(),lease_until=NULL,error_code='',completed_at=NULL WHERE batch_id=$1 AND status='failed'`, batchID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrConflict
	}
	_, err = s.pool.Exec(ctx, `UPDATE certificate_batches SET status='queued',completed_at=NULL WHERE id=$1`, batchID)
	if err != nil {
		return err
	}
	return s.refreshBatch(ctx, batchID)
}

func (s *PostgresStore) FinalizeAttendance(ctx context.Context, eventID, by uuid.UUID) (time.Time, error) {
	var finalized time.Time
	err := s.pool.QueryRow(ctx, `
		INSERT INTO certificate_event_state (event_id,attendance_finalized_at,attendance_finalized_by)
		VALUES ($1,now(),$2)
		ON CONFLICT (event_id) DO UPDATE SET attendance_finalized_at=EXCLUDED.attendance_finalized_at,attendance_finalized_by=EXCLUDED.attendance_finalized_by,updated_at=now()
		RETURNING attendance_finalized_at`, eventID, nullableUUID(by)).Scan(&finalized)
	return finalized, err
}

func (s *PostgresStore) AttendanceFinalizedAt(ctx context.Context, eventID uuid.UUID) (*time.Time, error) {
	var finalized time.Time
	err := s.pool.QueryRow(ctx, `SELECT attendance_finalized_at FROM certificate_event_state WHERE event_id=$1`, eventID).Scan(&finalized)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return &finalized, err
}

func (s *PostgresStore) JobCounts(ctx context.Context, eventID uuid.UUID) (queued, failed int, err error) {
	err = s.pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status IN ('queued','running')),count(*) FILTER (WHERE status='failed') FROM certificate_jobs WHERE event_id=$1`, eventID).Scan(&queued, &failed)
	return
}

func (s *PostgresStore) refreshBatch(ctx context.Context, batchID uuid.UUID) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE certificate_batches b SET
			queued_count=x.queued_count,issued_count=x.issued_count,failed_count=x.failed_count,
			status=CASE WHEN x.issued_count=x.total_count THEN 'completed' WHEN x.queued_count=0 AND x.failed_count=x.total_count THEN 'failed' WHEN x.queued_count=0 THEN 'partial' ELSE 'running' END,
			completed_at=CASE WHEN x.queued_count=0 THEN now() ELSE NULL END
		FROM (SELECT batch_id,count(*) AS total_count,count(*) FILTER (WHERE status IN ('queued','running')) AS queued_count,count(*) FILTER (WHERE status='issued') AS issued_count,count(*) FILTER (WHERE status='failed') AS failed_count FROM certificate_jobs WHERE batch_id=$1 GROUP BY batch_id) x
		WHERE b.id=x.batch_id`, batchID)
	return err
}

func batchScan(b *Batch) []any {
	return []any{&b.ID, &b.EventID, &b.TemplateVersionID, &b.TemplateSource, &b.Reason, &b.Status, &b.RequestedBy, &b.TotalCount, &b.QueuedCount, &b.IssuedCount, &b.FailedCount, &b.CreatedAt, &b.StartedAt, &b.CompletedAt}
}

func jobScan(j *Job) []any {
	return []any{&j.ID, &j.BatchID, &j.EventID, &j.TicketID, &j.Status, &j.AttemptCount, &j.NextAttemptAt, &j.LeaseUntil, &j.ErrorCode, &j.CertificateID, &j.CreatedAt, &j.StartedAt, &j.CompletedAt}
}

func nullableUUID(id uuid.UUID) any {
	if id == uuid.Nil {
		return nil
	}
	return id
}

func nullableUUIDPtr(id *uuid.UUID) any {
	if id == nil || *id == uuid.Nil {
		return nil
	}
	return *id
}
