package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

// The SkyMail templates of the two account deletion notices
// (docs/account-lifecycle.md, "Deletion notices"): the person's request was
// received, and their account and data were deleted. Both are System account
// templates seeded from skymail-frontend.
const (
	DefaultAccountDeletionReceivedTemplateKey  = "account.deletion-requested"
	DefaultAccountDeletionCompletedTemplateKey = "account.deletion-completed"
)

const (
	kindAccountDeletionNotice = "account_deletion_notice"
	// reasonEraseAfterSendUnconfirmed: SkyMail took the notice but did not
	// say it keeps no record of it, an image from before erase_after_send.
	reasonEraseAfterSendUnconfirmed = "erase_after_send_unconfirmed"
)

// ErrEraseAfterSendUnconfirmed is a notice SkyMail took without saying it
// keeps no record of its recipient: an image from before erase_after_send
// keeps the row, with the address, until someone erases it.
var ErrEraseAfterSendUnconfirmed = errors.New("mail: SkyMail took the notice without erase_after_send")

// maxCreatedBody caps what is read of SkyMail's answer to a notice.
const maxCreatedBody = 4 << 10

// AccountDeletionNotice posts one account deletion notice to one address,
// by template key, with no recipient name and with erase_after_send:
// SkyMail sends it untracked and erases its row once it is sent or failed
// (skymail-backend README, "Kayıt bırakmayan posta"), since the person's
// Erasure command has run already and nothing would erase it later.
//
// It reports what SkyMail answered: nil when it took the mail and said it
// keeps no record, ErrEraseAfterSendUnconfirmed when it took the mail
// without saying so, ErrMaybeDelivered when the request went out and no
// answer came, a *SendError when it surely took nothing. The line it writes
// on a refusal carries no recipient, as every SkyMail line.
func (s *SkyMail) AccountDeletionNotice(ctx context.Context, templateKey, recipient string, vars map[string]string) error {
	templateKey = strings.TrimSpace(templateKey)
	if s == nil || templateKey == "" || strings.TrimSpace(s.BaseURL) == "" {
		return ErrTemplateUnconfigured
	}
	if s.Tokens == nil {
		return &SendError{}
	}
	token, err := s.Tokens.Token(ctx)
	if err != nil || token == "" {
		return &SendError{}
	}
	task := template{key: templateKey}.task(recipient, "", vars)
	task["erase_after_send"] = true
	body, err := json.Marshal(task)
	if err != nil {
		return &SendError{}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(s.BaseURL, "/")+"/v1/mail_tasks/single", bytes.NewReader(body))
	if err != nil {
		return &SendError{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	client := s.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		if neverConnected(err) {
			return &SendError{}
		}
		return ErrMaybeDelivered
	}
	defer resp.Body.Close()
	if !succeeded(resp.StatusCode) {
		warn(s.Logger, eventCallFailed, kindAccountDeletionNotice, resp.StatusCode, sendReason(resp.StatusCode), diagnosticCode(diagnosticBody(resp.Body)))
		return &SendError{Status: resp.StatusCode}
	}
	var created struct {
		EraseAfterSend bool `json:"erase_after_send"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxCreatedBody))
	if json.Unmarshal(raw, &created) != nil || !created.EraseAfterSend {
		warn(s.Logger, eventCallFailed, kindAccountDeletionNotice, resp.StatusCode, reasonEraseAfterSendUnconfirmed, "")
		return ErrEraseAfterSendUnconfirmed
	}
	return nil
}
