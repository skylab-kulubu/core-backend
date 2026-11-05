package main

import (
	"fmt"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/mail"
)

// Variables of the account deletion notices (docs/account-lifecycle.md,
// "Deletion notices").
const (
	accountDeletionNoticesEnv              = "ACCOUNT_DELETION_NOTICES"
	accountDeletionReceivedTemplateKeyEnv  = "SKYMAIL_ACCOUNT_DELETION_RECEIVED_TEMPLATE_KEY"
	accountDeletionCompletedTemplateKeyEnv = "SKYMAIL_ACCOUNT_DELETION_COMPLETED_TEMPLATE_KEY"
)

// accountDeletionNotices reads whether the erasure worker sends the
// deletion notices. ACCOUNT_DELETION_NOTICES is off unless it is "on": a
// notice needs a SkyMail that keeps no record of it (erase_after_send) and
// its templates seeded, so it is turned on once both are live. The template
// keys default to the seeded ones; an empty key turns that notice off. nil
// is off.
func accountDeletionNotices(lookup func(string) (string, bool), claims account.DeletionNoticeClaims, mailer account.DeletionNoticeMailer) (*account.DeletionNotices, error) {
	raw, _ := lookup(accountDeletionNoticesEnv)
	switch strings.TrimSpace(raw) {
	case "", "off":
		return nil, nil
	case "on":
	default:
		return nil, fmt.Errorf("%s must be on or off", accountDeletionNoticesEnv)
	}
	return &account.DeletionNotices{
		Claims:               claims,
		Mailer:               mailer,
		ReceivedTemplateKey:  templateKey(lookup, accountDeletionReceivedTemplateKeyEnv, mail.DefaultAccountDeletionReceivedTemplateKey),
		CompletedTemplateKey: templateKey(lookup, accountDeletionCompletedTemplateKeyEnv, mail.DefaultAccountDeletionCompletedTemplateKey),
	}, nil
}
