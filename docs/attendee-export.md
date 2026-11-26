# Attendee export

The panel downloads a report of an Event's attendees as an Excel workbook
(XLSX) or a CSV file: `POST /v1/events/{eventId}/exports`. Core makes the
file in memory and keeps it nowhere; every download is a row of the Team
audit record (`ticket.export`, [team-audit.md](team-audit.md)).

## Who

A reader of the Event's Tickets (`ticket.read`: Privileged and Privileged
readers, the organizer teams' Leaders, who may edit the Event, a member who
issues certificates, a Co-organizer person). The Event's door staff check
people in and read no list: they are refused (403), as on
`GET /v1/events/{id}/tickets`. A draft the caller may not see is 404.

The certificate report and the certificate status column need
`certificate.read` on the Event as well; without it the report is 403 and
the column is left out.

## Request

```json
{"report": "attendees", "format": "xlsx", "sessionId": null, "ticketIds": ["…"]}
```

- `report`: `attendees`, `session`, `matrix`, `entries` or `certificates`.
- `format`: `xlsx` (default) or `csv`.
- `sessionId`: the Session of a `session` report, and only of it.
- `ticketIds` (optional): the rows the panel shows ("ekrandakini indir").
  Present, the file has those Tickets alone, in that order (at most
  20 000); ids that are not the Event's are left out. Absent, the whole
  Event.

400 `invalid_export` for anything else; 503 `audit_unavailable` when the
audit row cannot be opened (nothing is downloaded).

## Answer

`200` with the file: `Content-Type` the format's, `Content-Disposition:
attachment; filename="<event>-<report>-<YYYYMMDD-HHMM>.<format>"` (the
Event's name in lower-case ASCII, Istanbul's clock), `Cache-Control:
no-store`, `X-Content-Type-Options: nosniff`.

- XLSX: one sheet, Turkish headers, the header row frozen and an auto
  filter on it; dates and times are date cells (Istanbul's wall clock,
  `dd.mm.yyyy hh:mm`); every text is a text cell, never a formula. Made with
  [excelize](https://github.com/xuri/excelize) (BSD-3-Clause) v2.10.1;
  v2.11.0 would also raise `golang.org/x/image` (0.34 → 0.38, which the
  ticket page, QR and certificate code draw with) and `mscfb`, so it waits
  for a release of its own.
- CSV: UTF-8 with a byte order mark, semicolon-separated, CRLF (how Excel
  opens it on a Turkish system); dates as `26.11.2026 14:05`; a text that
  starts with `= + - @`, a tab or a carriage return gets a leading
  apostrophe (OWASP CSV injection).

## Reports

The person columns open every report: Ad, Soyad, E-posta, Üye/Misafir. A
member's name and address are their account's; an erased account writes
"Silinmiş kullanıcı" and nothing else. The reports about the people
themselves (`attendees`, `session`, `entries`) follow them with Telefon,
Üniversite, Bölüm and Sınıf: a guest's phone, university, department and
grade as the Ticket holds them (a Walk-in's, [walk-in.md](walk-in.md)); a
member's university and department from their account, no phone (the
account's phone is not shown to organizers). The grade is written as the
panel shows it (Hazırlık, 1.–4. sınıf, Yüksek lisans, Doktora, Mezun, Diğer). The
phone is a text cell in the XLSX; in the CSV a leading `+` gets the
apostrophe below, so Excel keeps it as text, and the apostrophe shows in
the cell (`'+905551112233`): the XLSX is the format to read phones in
(Yusuf, 2026-11-26: the phone is exported).

| Report | Rows | Columns after the person |
| --- | --- | --- |
| `attendees` (Katılımcılar) | every Ticket, cancelled and waitlisted included, by registration | Telefon, Üniversite, Bölüm, Sınıf, Bilet kodu, Bilet durumu (Aktif / Bekleme listesi / İptal), Kayıt kaynağı (Form / Kapıda kayıt / Doğrudan), Kayıt tarihi, Alana giriş (gün), Alana giriş zamanları, Katıldığı oturum sayısı, then the counts |
| `session` (Oturum katılımı) | the Tickets with a Check-in on the Session, by name | Telefon, Üniversite, Bölüm, Sınıf, Bilet durumu, Katılım zamanı, Oturum |
| `matrix` (Katılım matrisi) | the active Tickets, by name | one column per live Session in the program's order ("(sayılır)" when counted), ✓ where checked in; Toplam katıldığı oturum, then the counts |
| `entries` (Alana giriş) | every Venue entry, day by day | Gün first; Telefon, Üniversite, Bölüm, Sınıf, Giriş zamanı, Cihazda okutma zamanı (an offline scan's), Yol (QR okutma / Kapı listesi (elle) / Oturum katılımıyla / Kapıda kayıt) |
| `certificates` (Sertifikalar) | the active Tickets, by name | Sayılan oturumlardan katıldığı, Sayılan oturum sayısı, Sertifika durumu, Seri no, Veriliş, İptal |

The counts are the certificate rule's ([certificate-rule.md](certificate-rule.md)),
read once per export by `certificate.Service.Standings`, the same count the
certificate list and the draw read: Sayılan oturumlardan katıldığı,
Sayılan oturum sayısı, Sertifika eşiğini geçiyor (Evet / Hayır, or "Kural
yok" for an Event without a rule) and, for a reader of certificates,
Sertifika durumu (Verildi, Kuyrukta, Hata, İptal edildi, Uygun verilmedi,
Eşik altı). A Ticket that is not active has none.

Not written: a Session Check-in's way (QR, door, by hand) and who wrote
it, which core does not keep; a Venue entry's door staff; the form's
answers beyond the fields it maps onto the Ticket.

## Audit and log

The row (`ticket.export`, the Event's team) is opened before the file is
made and closed with `export`: `report`, `format`, `selected`, a session
report's `sessionId` and the file's `rows`; `failed` when the file could not
be made. A reader who is neither Privileged nor a Leader is recorded as an
`event_editor` through the Event's team, as an import is (migration
`20261127223000`). One log line per export, `attendees_exported`: the
correlation id, the Event, the report, the format, `selected` and the row
count; never a name, an address or who asked.

## Size

The export reads the Event's Tickets with their Check-ins (two queries),
their members' accounts (one), the program, the Venue entries day by day
and the certificate attendance (two), whatever the number of Tickets; the
XLSX is written with excelize's stream writer. A 2 000-Ticket Event's
workbook is made in well under a second.
