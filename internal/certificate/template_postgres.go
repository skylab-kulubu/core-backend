package certificate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/skylab-kulubu/core-backend/internal/media"
	"github.com/skylab-kulubu/core-backend/internal/subjectlock"
)

const templateCols = `id, name, owner_team, source_kind, source_ref, source_edit_url, draft_layout, system, archived_at, created_by, created_at, updated_at`
const versionCols = `id, template_id, version, layout, asset_manifest, checksum, published_by, published_at`

func (s *PostgresStore) ListTemplates(ctx context.Context) ([]Template, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+templateCols+` FROM certificate_templates WHERE archived_at IS NULL ORDER BY system DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Template, 0)
	for rows.Next() {
		t, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	for i := range out {
		if version, err := s.LatestVersion(ctx, out[i].ID); err == nil {
			out[i].PublishedVersion = &version
		} else if !errors.Is(err, ErrNotFound) {
			return nil, err
		}
	}
	return out, nil
}

func (s *PostgresStore) GetTemplate(ctx context.Context, id uuid.UUID) (Template, error) {
	t, err := scanTemplate(s.pool.QueryRow(ctx, `SELECT `+templateCols+` FROM certificate_templates WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if err != nil {
		return Template{}, err
	}
	if version, versionErr := s.LatestVersion(ctx, id); versionErr == nil {
		t.PublishedVersion = &version
	} else if !errors.Is(versionErr, ErrNotFound) {
		return Template{}, versionErr
	}
	return t, nil
}

func (s *PostgresStore) CreateTemplate(ctx context.Context, t Template) (Template, error) {
	if t.ID == uuid.Nil {
		t.ID = uuid.New()
	}
	raw, err := marshalLayout(t.DraftLayout)
	if err != nil {
		return Template{}, err
	}
	created, err := scanTemplate(s.pool.QueryRow(ctx, `
		INSERT INTO certificate_templates (id,name,owner_team,source_kind,source_ref,source_edit_url,draft_layout,system,created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9) RETURNING `+templateCols,
		t.ID, t.Name, t.OwnerTeam, t.SourceKind, t.SourceRef, t.SourceEditURL, raw, t.System, t.CreatedBy))
	if subjectlock.IsInactiveAccountReference(err) {
		return Template{}, ErrForbidden
	}
	if refusal, ok := media.DatabaseLinkRefusal(err); ok {
		return Template{}, refusal
	}
	return created, err
}

func (s *PostgresStore) UpdateTemplate(ctx context.Context, t Template) (Template, error) {
	raw, err := marshalLayout(t.DraftLayout)
	if err != nil {
		return Template{}, err
	}
	updated, err := scanTemplate(s.pool.QueryRow(ctx, `
		UPDATE certificate_templates SET name=$2,owner_team=$3,source_kind=$4,source_ref=$5,source_edit_url=$6,draft_layout=$7,updated_at=now()
		WHERE id=$1 AND archived_at IS NULL RETURNING `+templateCols,
		t.ID, t.Name, t.OwnerTeam, t.SourceKind, t.SourceRef, t.SourceEditURL, raw))
	if errors.Is(err, pgx.ErrNoRows) {
		return Template{}, ErrNotFound
	}
	if refusal, ok := media.DatabaseLinkRefusal(err); ok {
		return Template{}, refusal
	}
	return updated, err
}

func (s *PostgresStore) CreateVersion(ctx context.Context, v TemplateVersion) (TemplateVersion, error) {
	if v.ID == uuid.Nil {
		v.ID = uuid.New()
	}
	raw, err := marshalLayout(v.Layout)
	if err != nil {
		return TemplateVersion{}, err
	}
	assets, err := json.Marshal(v.AssetManifest)
	if err != nil {
		return TemplateVersion{}, err
	}
	err = s.pool.QueryRow(ctx, `
		INSERT INTO certificate_template_versions (id,template_id,version,layout,asset_manifest,checksum,published_by,asset_serving_policy_applied)
		VALUES ($1,$2,(SELECT COALESCE(MAX(version),0)+1 FROM certificate_template_versions WHERE template_id=$2),$3,$4,$5,$6,$7)
		RETURNING `+versionCols, v.ID, v.TemplateID, raw, assets, v.Checksum, v.PublishedBy, v.AssetServingPolicyApplied).Scan(
		&v.ID, &v.TemplateID, &v.Version, &raw, &assets, &v.Checksum, &v.PublishedBy, &v.PublishedAt,
	)
	if isUnique(err) {
		return TemplateVersion{}, ErrConflict
	}
	if subjectlock.IsInactiveAccountReference(err) {
		return TemplateVersion{}, ErrForbidden
	}
	if refusal, ok := media.DatabaseLinkRefusal(err); ok {
		return TemplateVersion{}, refusal
	}
	if err == nil {
		err = json.Unmarshal(raw, &v.Layout)
	}
	if err == nil {
		err = json.Unmarshal(assets, &v.AssetManifest)
	}
	return v, err
}

func (s *PostgresStore) ListPendingAssetServingPolicy(ctx context.Context, after uuid.UUID, limit int) ([]TemplateVersion, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, asset_manifest FROM certificate_template_versions
		WHERE asset_serving_policy_applied = false AND id > $1 ORDER BY id LIMIT $2`, after, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TemplateVersion, 0)
	for rows.Next() {
		var v TemplateVersion
		var assets []byte
		if err := rows.Scan(&v.ID, &assets); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(assets, &v.AssetManifest); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetAssetServingPolicyApplied touches only the flag, so the guards on a
// version's layout and asset manifest do not run again.
func (s *PostgresStore) SetAssetServingPolicyApplied(ctx context.Context, id uuid.UUID) error {
	tag, err := s.pool.Exec(ctx, `UPDATE certificate_template_versions SET asset_serving_policy_applied = true WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) LatestVersion(ctx context.Context, templateID uuid.UUID) (TemplateVersion, error) {
	return scanVersion(s.pool.QueryRow(ctx, `SELECT `+versionCols+` FROM certificate_template_versions WHERE template_id=$1 ORDER BY version DESC LIMIT 1`, templateID))
}

func (s *PostgresStore) GetVersion(ctx context.Context, id uuid.UUID) (TemplateVersion, error) {
	return scanVersion(s.pool.QueryRow(ctx, `SELECT `+versionCols+` FROM certificate_template_versions WHERE id=$1`, id))
}

func (s *PostgresStore) GetBinding(ctx context.Context, scope, scopeKey string) (Binding, error) {
	var b Binding
	err := s.pool.QueryRow(ctx, `SELECT id,scope,scope_key,template_id,updated_by,updated_at FROM certificate_template_bindings WHERE scope=$1 AND scope_key=$2`, scope, scopeKey).Scan(&b.ID, &b.Scope, &b.ScopeKey, &b.TemplateID, &b.UpdatedBy, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Binding{}, ErrNotFound
	}
	return b, err
}

func (s *PostgresStore) ListBindings(ctx context.Context) ([]Binding, error) {
	rows, err := s.pool.Query(ctx, `SELECT id,scope,scope_key,template_id,updated_by,updated_at FROM certificate_template_bindings ORDER BY scope,scope_key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]Binding, 0)
	for rows.Next() {
		var binding Binding
		if err := rows.Scan(&binding.ID, &binding.Scope, &binding.ScopeKey, &binding.TemplateID, &binding.UpdatedBy, &binding.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, binding)
	}
	return out, rows.Err()
}

func (s *PostgresStore) SetBinding(ctx context.Context, b Binding) (Binding, error) {
	if b.ID == uuid.Nil {
		b.ID = uuid.New()
	}
	err := s.pool.QueryRow(ctx, `
		INSERT INTO certificate_template_bindings (id,scope,scope_key,template_id,updated_by)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (scope,scope_key) DO UPDATE SET template_id=EXCLUDED.template_id,updated_by=EXCLUDED.updated_by,updated_at=now()
		RETURNING id,scope,scope_key,template_id,updated_by,updated_at`,
		b.ID, b.Scope, b.ScopeKey, b.TemplateID, b.UpdatedBy).Scan(&b.ID, &b.Scope, &b.ScopeKey, &b.TemplateID, &b.UpdatedBy, &b.UpdatedAt)
	if subjectlock.IsInactiveAccountReference(err) {
		return Binding{}, ErrForbidden
	}
	return b, err
}

func (s *PostgresStore) DeleteBinding(ctx context.Context, scope, scopeKey string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM certificate_template_bindings WHERE scope=$1 AND scope_key=$2`, scope, scopeKey)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanTemplate(row rowScanner) (Template, error) {
	var t Template
	var raw []byte
	err := row.Scan(&t.ID, &t.Name, &t.OwnerTeam, &t.SourceKind, &t.SourceRef, &t.SourceEditURL, &raw, &t.System, &t.ArchivedAt, &t.CreatedBy, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return Template{}, err
	}
	if err := json.Unmarshal(raw, &t.DraftLayout); err != nil {
		return Template{}, err
	}
	return t, nil
}

func scanVersion(row rowScanner) (TemplateVersion, error) {
	var v TemplateVersion
	var raw, assets []byte
	err := row.Scan(&v.ID, &v.TemplateID, &v.Version, &raw, &assets, &v.Checksum, &v.PublishedBy, &v.PublishedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return TemplateVersion{}, ErrNotFound
	}
	if err != nil {
		return TemplateVersion{}, err
	}
	if err := json.Unmarshal(raw, &v.Layout); err != nil {
		return TemplateVersion{}, err
	}
	if err := json.Unmarshal(assets, &v.AssetManifest); err != nil {
		return TemplateVersion{}, err
	}
	return v, nil
}
