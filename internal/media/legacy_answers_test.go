package media_test

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// Skyforms' export is read as it comes from psql: a header, Windows line
// ends and blank or '#' lines are fine; a line it cannot read is named by its
// number, never by its content.
func TestReadLegacyAnswerPairs(t *testing.T) {
	t.Parallel()
	m, r := uuid.New(), uuid.New()
	pairs, err := media.ReadLegacyAnswerPairs(strings.NewReader("media_id\tresponse_id\r\n# Skyforms\n\n" + m.String() + "\t" + r.String() + "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0] != (media.LegacyAnswerPair{MediaID: m, ResponseID: r}) {
		t.Fatalf("pairs %+v", pairs)
	}
	for name, input := range map[string]string{
		"one column":       m.String() + "\n",
		"three columns":    m.String() + "\t" + r.String() + "\tx\n",
		"not a Media id":   "cv.pdf\t" + r.String() + "\n",
		"not a response":   m.String() + "\thttps://cdn.example.test/files/x.pdf\n",
		"nil Media id":     uuid.Nil.String() + "\t" + r.String() + "\n",
		"empty second col": m.String() + "\t\n",
	} {
		_, err := media.ReadLegacyAnswerPairs(strings.NewReader(input))
		if err == nil || !strings.Contains(err.Error(), "line 1") || strings.Contains(err.Error(), "cdn.example") || strings.Contains(err.Error(), "cv.pdf") {
			t.Errorf("%s: error %v", name, err)
		}
	}
}
