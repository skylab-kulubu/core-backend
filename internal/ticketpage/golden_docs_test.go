package ticketpage_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/skylab-kulubu/core-backend/internal/ticket/theme"
	"github.com/skylab-kulubu/core-backend/internal/ticketpage"
)

// kurumsalGoldenDocuments are the tickets whose Kurumsal HTML is pinned:
// the plain one, with a programme, a band colour, a picture band, a map and
// Co-organizers.
func kurumsalGoldenDocuments() []ticketpage.Document {
	plain := baseDocument()
	full := baseDocument()
	full.Days = programDays(3, 4)
	full.BandJPEG = []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	full.Map, full.MapType, full.MapAttribution = []byte{0x89, 'P', 'N', 'G'}, "image/png", "© OpenStreetMap katkıda bulunanlar"
	full.CoOrganizers = "WEBLAB · Web Geliştirme, ARTLAB"
	colour := baseDocument()
	colour.BandColor = "#0f766e"
	colour.Start, colour.End = at("2026-12-12T14:00:00+03:00"), nil
	colour.OwnerTeam = "YK"
	return []ticketpage.Document{plain, full, colour}
}

// Kurumsal's ticket is the same, byte for byte, as before Event themes
// (docs/event-theme.md): its document and its page head, hashed as main
// drew them on 2026-11-26 (c91a3ac5), with no theme and with Kurumsal's;
// the document's since the place's name, address and directions QR became
// links and the ticket note got its section (2026-11-26, the head unchanged).
func TestKurumsalTicketUnchanged(t *testing.T) {
	t.Parallel()
	want := []string{
		"b10f22c4b05cfc0c725fb7f7b75140625d455d99992cfbdeb6348051ae131ef4",
		"d6c985b0c84b4079d5a7324a147e2b9fa699ad72214169298b7fa5f33d8878e0",
		"bf77d057a1d16fef7cd0f274e3877d9586a2608803e904e4748c35b43dc54af1",
	}
	const head = "b07f3efc14b8a14eb95d29cda14a6ed494e1edd33c906f82cf9e1fb85463da60"
	for _, look := range []theme.Theme{{}, theme.Lookup(""), theme.Lookup("kurumsal"), theme.Lookup("bilinmeyen")} {
		for i, d := range kurumsalGoldenDocuments() {
			d.Theme = look
			html, header, err := ticketpage.RenderHTML(d)
			if err != nil {
				t.Fatal(err)
			}
			if got := sum(html); got != want[i] {
				t.Errorf("theme %q, document %d: %s", look.Key, i, got)
			}
			if got := sum(header); got != head {
				t.Errorf("theme %q, document %d: head %s", look.Key, i, got)
			}
		}
	}
}

func sum(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}
