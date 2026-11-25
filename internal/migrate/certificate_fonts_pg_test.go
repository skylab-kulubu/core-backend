package migrate_test

import (
	"context"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

// undoCertificateFonts rolls back the certificate fonts migration, whose
// triggers use the Media attachments' function and the account guard and
// whose role tables an older down replaces, so that an older down
// migration can run; applying again brings it back.
func undoCertificateFonts(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), readDown(t, certificateFontsDown)); err != nil {
		t.Fatalf("certificate fonts down: %v", err)
	}
	if _, err := pool.Exec(context.Background(), `DELETE FROM schema_migrations WHERE version = `+certificateFontsVersion); err != nil {
		t.Fatal(err)
	}
}

const (
	certificateFontsVersion = "20261127183000"
	certificateFontsDown    = certificateFontsVersion + "_certificate_fonts.down.sql"
)

// Certificate fonts (docs/certificate-fonts.md): up makes the table, its
// one-current-face index and its Media attachment, counts an element's
// fontMediaId among a layout's Media and lets a layout link a font; down
// takes it all back (the font's attachment too); up again is as before.
func TestCertificateFontsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	background, image, font := uuid.New(), uuid.New(), uuid.New()
	layout := `{"backgroundMediaId":"` + background.String() + `","elements":[{"id":"logo","kind":"image","mediaId":"` + image.String() +
		`"},{"id":"t","kind":"staticText","fontMediaId":"` + font.String() + `"}]}`
	linked := func() []string {
		t.Helper()
		var ids []string
		if err := pool.QueryRow(ctx, `SELECT certificate_layout_media_ids($1::jsonb, '{}'::jsonb)::text[]`, layout).Scan(&ids); err != nil {
			t.Fatal(err)
		}
		slices.Sort(ids)
		return ids
	}
	want := func(ids ...uuid.UUID) []string {
		out := make([]string, len(ids))
		for i, id := range ids {
			out[i] = id.String()
		}
		slices.Sort(out)
		return out
	}
	fits := func() (bool, bool, bool) {
		t.Helper()
		var layoutFont, fontRole, legacy bool
		// The simple protocol: a cached plan would keep the immutable
		// function's answer from before the down migration.
		if err := pool.QueryRow(ctx, `SELECT media_purpose_fits_role('core', 'certificate_asset', 'certificate_font'),
			media_purpose_fits_role('core', 'certificate_font', 'certificate_font'),
			media_purpose_fits_role('core', 'certificate_font', 'legacy')`, pgx.QueryExecModeSimpleProtocol).Scan(&layoutFont, &fontRole, &legacy); err != nil {
			t.Fatal(err)
		}
		return layoutFont, fontRole, legacy
	}
	if got := linked(); !slices.Equal(got, want(background, image, font)) {
		t.Fatalf("up: %v", got)
	}
	if layoutFont, fontRole, legacy := fits(); !layoutFont || !fontRole || legacy {
		t.Fatalf("up fits: %v %v %v", layoutFont, fontRole, legacy)
	}

	// A font row links its file; the same face twice in a team is refused
	// while the first is current, kept once it is archived; another team
	// has its own.
	mediaID := uuid.New()
	if _, err := pool.Exec(ctx, `
		INSERT INTO media (id, file_name, file_type, file_url, file_size, kind, purpose, status)
		VALUES ($1, 'f.woff2', 'font/woff2', $2, 8, 'FILE', 'certificate_font', 'pending')`, mediaID, "files/"+mediaID.String()); err != nil {
		t.Fatal(err)
	}
	insert := func(team, family string) error {
		_, err := pool.Exec(ctx, `INSERT INTO certificate_fonts (id, owner_team, family, weight, style, media_id) VALUES ($1, $2, $3, 700, 'normal', $4)`,
			uuid.New(), team, family, mediaID)
		return err
	}
	if err := insert("ARTLAB", "Brandon Grotesque"); err != nil {
		t.Fatal(err)
	}
	var attachments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments WHERE media_id = $1 AND owner_type = 'certificate_font' AND role = 'certificate_font'`, mediaID).Scan(&attachments); err != nil || attachments != 1 {
		t.Fatalf("attachments %d %v", attachments, err)
	}
	if err := insert("ARTLAB", "brandon grotesque"); err == nil {
		t.Fatal("the same face twice")
	}
	if err := insert("GECEKODU", "Brandon Grotesque"); err != nil {
		t.Fatalf("another team: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE certificate_fonts SET archived_at = now() WHERE owner_team = 'ARTLAB'`); err != nil {
		t.Fatal(err)
	}
	if err := insert("ARTLAB", "Brandon Grotesque"); err != nil {
		t.Fatalf("after archive: %v", err)
	}
	if !recorded(t, pool, certificateFontsVersion) {
		t.Fatal("not recorded")
	}

	if _, err := pool.Exec(ctx, readDown(t, certificateFontsDown)); err != nil {
		t.Fatal(err)
	}
	var table *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.certificate_fonts')::text`).Scan(&table); err != nil || table != nil {
		t.Fatalf("down: table %v %v", table, err)
	}
	if got := linked(); !slices.Equal(got, want(background, image)) {
		t.Fatalf("down: %v", got)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM media_attachments WHERE owner_type = 'certificate_font'`).Scan(&attachments); err != nil || attachments != 0 {
		t.Fatalf("down: attachments %d %v", attachments, err)
	}
	if layoutFont, fontRole, _ := fits(); layoutFont || fontRole {
		t.Fatalf("down fits: %v %v", layoutFont, fontRole)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+certificateFontsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if got := linked(); !slices.Equal(got, want(background, image, font)) {
		t.Fatalf("up again: %v", got)
	}
	if layoutFont, fontRole, legacy := fits(); !layoutFont || !fontRole || legacy {
		t.Fatalf("up again fits: %v %v %v", layoutFont, fontRole, legacy)
	}
}
