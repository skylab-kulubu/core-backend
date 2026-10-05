package retention

import "github.com/skylab-kulubu/core-backend/internal/consent"

// consentRules are the contact consent rules (ADR-0062,
// docs/contact-consents.md). A grant nobody confirmed goes once its last
// confirmation link has expired: a pending one, and a superseded one (a
// pending grant a verified grant replaced), which was never consent and so
// is no proof. A renewal question nobody answered (no renewal, no check-in
// since) ends the grant as expired, clearing its address. Any other ended
// grant is proof, kept three years after its end.
func consentRules(s Schema) []Rule {
	if !s.ContactConsents {
		return nil
	}
	return []Rule{
		{
			Name: "consent_pending", Version: 1, Kind: KindSweep, Action: ActionDelete,
			Table: "contact_consents", alias: "c", key: "id", Period: consent.PendingTTL,
			// The link a confirmation mail carries works PendingTTL from that
			// mail, so the row stays as long as a link to it works (a
			// superseded grant's link says the consent is already recorded).
			where: `c.confirmed_at IS NULL AND (c.ended_at IS NULL OR c.ended_reason = 'superseded')
	AND COALESCE(c.confirmation_sent_at, c.granted_at) < $1`,
		},
		{
			Name: "consent_renewal_unanswered", Version: 1, Kind: KindSweep, Action: ActionScrub,
			Table: "contact_consents", alias: "c", key: "id", Period: consent.RenewalAnswerWindow,
			where: `c.ended_at IS NULL AND c.confirmed_at IS NOT NULL AND c.renewal_requested_at < $1
	AND ` + consent.RenewalAnchorSQL + ` < c.renewal_requested_at`,
			set: `ended_at = now(), ended_reason = 'expired', ended_via = 'renewal_unanswered', email = NULL`,
		},
		{
			Name: "consent_proof", Version: 1, Kind: KindSweep, Action: ActionDelete,
			Table: "contact_consents", alias: "c", key: "id", Period: consent.ProofRetention,
			where: `c.ended_at IS NOT NULL AND c.ended_reason <> 'superseded' AND c.ended_at < $1`,
		},
	}
}
