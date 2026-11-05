package main

import (
	"fmt"
	"io"
)

// The team page notice (docs/team-page-consent.md, "The notice") told
// undecided members that the team pages would hide them on a date. Since
// 2026-10-31 every member is shown unless they object (#339), so that mail
// would be wrong: the command refuses every run, dry or -apply, whatever the
// environment holds, and reads or writes nothing (no Keycloak, database or
// SkyMail). It stays as a name so that callers get a clear refusal (exit 2)
// instead of the server starting. The ledger team_page_notices and its
// account erasure stay in internal/teampagenotice.
const teamPageNoticeCommandName = "team-page-notice"

func runTeamPageNotice(_ []string, _ func(string) string, _, errOut io.Writer) int {
	fmt.Fprintf(errOut, "%s: ekip sayfaları varsayılan açık; bu bildirim artık gönderilmez (team pages are shown by default; this notice is no longer sent)\n", teamPageNoticeCommandName)
	return 2
}
