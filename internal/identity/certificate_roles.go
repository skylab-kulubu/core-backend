package identity

import (
	"fmt"
	"strings"
)

// CertificateClientRoles are the client roles of core's Keycloak client that certificate
// authorization reads from tokens (internal/authz). Keycloak owns them: the operator script
// below creates missing ones, core only checks at startup that they exist.
var CertificateClientRoles = []string{
	"certificate:template:manage",
	"certificate:binding:manage",
	"certificate:issue",
	"certificate:revoke",
}

// CertificateRolesOperatorCommand is what an operator runs inside the Keycloak container to
// create missing certificate roles (e-skylab-keycloak, docs/v2-identity-reconcile-runbook.md §8).
const CertificateRolesOperatorCommand = "/opt/keycloak/config/identity-guardrails.sh --admin-user <admin> --apply"

// CertificateRolesWarning turns the startup role check into one log line, or "" when every
// certificate role exists. Missing roles do not stop core: certificate actions that need them
// are denied until the roles exist and are granted.
func CertificateRolesWarning(clientID string, missing []string, err error) string {
	if err != nil {
		return fmt.Sprintf("certificate client roles could not be checked on Keycloak client %s: %v", clientID, err)
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"certificate client roles missing on Keycloak client %s: %s; core does not create client roles, run the Keycloak operator script: %s",
		clientID, strings.Join(missing, ", "), CertificateRolesOperatorCommand)
}
