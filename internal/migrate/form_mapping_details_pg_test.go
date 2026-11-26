package migrate_test

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/migrate"
)

const (
	mappingDetailsVersion = "20261128010000"
	mappingDetailsDown    = mappingDetailsVersion + "_form_mapping_details.down.sql"
)

// A form mapping's details: the checks hold (a blank or over-long question
// id, a respondent row's unknown grade, a signed-in person's row with a
// detail), the up migration runs again over itself, and down keeps the
// columns and their values.
func TestFormMappingDetailsMigration(t *testing.T) {
	pool := postgresPool(t)
	ctx := context.Background()
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	eventID, formID := uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO events (id, name, location, owner_team) VALUES ($1, 'Hack', 'YTÜ', 'WEBLAB')`, eventID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO event_form_mappings (event_id, form_id, full_name_question, email_question, phone_question, grade_question)
		VALUES ($1, $2, 'n', 'e', 'p', 'g')`, eventID, formID); err != nil {
		t.Fatal(err)
	}
	for name, column := range map[string]string{"blank": " ", "too long": strings.Repeat("q", 101)} {
		if _, err := pool.Exec(ctx, `UPDATE event_form_mappings SET university_question = $1`, column); err == nil {
			t.Errorf("%s question id taken", name)
		}
	}
	row := func(userID any, grade, phone string) error {
		_, err := pool.Exec(ctx, `
			INSERT INTO event_form_respondents (id, event_id, form_id, form_response_id, user_id, first_name, last_name, email, forms_status, source, grade, phone_number)
			VALUES ($1, $2, $3, $4, $5, CASE WHEN $5::uuid IS NULL THEN 'Ada' END, CASE WHEN $5::uuid IS NULL THEN 'Lovelace' END,
				CASE WHEN $5::uuid IS NULL THEN 'ada@example.com' END, 'accepted', 'collect', $6, $7)`,
			uuid.New(), eventID, formID, uuid.New(), userID, grade, phone)
		return err
	}
	if err := row(nil, "3", "+905551112233"); err != nil {
		t.Fatal(err)
	}
	if err := row(nil, "5", ""); err == nil {
		t.Error("an unknown grade taken")
	}
	person := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, email) VALUES ($1, 'person@example.com')`, person); err != nil {
		t.Fatal(err)
	}
	if err := row(person, "", "+905551112233"); err == nil {
		t.Error("a signed-in person's row took a detail")
	}
	if err := row(person, "", ""); err != nil {
		t.Fatal(err)
	}

	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mappingDetailsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
	if !recorded(t, pool, mappingDetailsVersion) {
		t.Fatal("not recorded")
	}
	if _, err := pool.Exec(ctx, readDown(t, mappingDetailsDown)); err != nil {
		t.Fatal(err)
	}
	var phone, grade string
	if err := pool.QueryRow(ctx, `SELECT phone_question, grade_question FROM event_form_mappings WHERE event_id = $1`, eventID).Scan(&phone, &grade); err != nil || phone != "p" || grade != "g" {
		t.Fatalf("after down: %q %q %v", phone, grade, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE version = `+mappingDetailsVersion); err != nil {
		t.Fatal(err)
	}
	if err := migrate.Apply(ctx, pool); err != nil {
		t.Fatal(err)
	}
}
