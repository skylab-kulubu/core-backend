package certificate

import (
	"encoding/base64"
	"html"

	"github.com/skylab-kulubu/core-backend/internal/organizer"
)

func HTML(ownerTeam, recipientName, eventName, verifyURL string, qrPNG []byte) string {
	// Headed with the organizer as shown everywhere (organizer.Name): the
	// club's name for YK, DK, ADMIN or no Owner team, else "KOD · Türkçe ad".
	heading := html.EscapeString(organizer.Name(ownerTeam))
	qr := ""
	if len(qrPNG) > 0 {
		qr = `<img alt="verify" src="data:image/png;base64,` + base64.StdEncoding.EncodeToString(qrPNG) + `" width="120" height="120"/>`
	}
	return `<!DOCTYPE html>
<html lang="tr">
<head>
<meta charset="utf-8"/>
<title>Sertifika</title>
<style>
  @page { size: A4 landscape; margin: 24px; }
  body { font-family: "Helvetica Neue", Helvetica, Arial, sans-serif; color: #111; }
  .sheet { border: 8px solid #111; padding: 48px; text-align: center; }
  h1 { letter-spacing: 0.2em; font-size: 14px; }
  h2 { font-size: 36px; margin: 24px 0 8px; }
  p { font-size: 16px; }
</style>
</head>
<body>
<div class="sheet">
  <h1>` + heading + `</h1>
  <p>Katılım sertifikası</p>
  <h2>` + html.EscapeString(recipientName) + `</h2>
  <p>` + html.EscapeString(eventName) + `</p>
  <p><a href="` + html.EscapeString(verifyURL) + `">` + html.EscapeString(verifyURL) + `</a></p>
  ` + qr + `
</div>
</body>
</html>`
}
