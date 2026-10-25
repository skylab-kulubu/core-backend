package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/account"
	"github.com/skylab-kulubu/core-backend/internal/consent"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The staff withdrawal (docs/contact-consents.md, "Withdrawal by mail"): a
// person asked by mail to info@yildizskylab.com (aydınlatma s6 §6) that their
// consent end. Yusuf runs it inside the core container, the address on
// standard input, so that it is in no process list, shell history or log:
//
//	core-backend consent-withdraw -purpose promotional,event_invitations < address
//
// It ends every open grant of the purposes of the person holding the
// address, as ended by staff (ended_via staff, the record of it), and prints
// how many per purpose; never the address. Running it again ends none.
const consentWithdrawCommandName = "consent-withdraw"

// Exit codes beyond 0 (done), 1 (core or Keycloak failed; nothing or, past
// the first purpose, part ended: run it again) and 2 (usage or
// configuration).
const consentWithdrawExitOff = 3

// runConsentWithdraw wires the withdrawal to the database and to Keycloak
// through core's service account, which find every address of the person.
func runConsentWithdraw(args []string, getenv func(string) string, in io.Reader, out, errOut io.Writer) int {
	purposes, code := parseConsentWithdrawPurposes(args, errOut)
	if code != 0 {
		return code
	}
	config, err := consent.ConfigFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", consentWithdrawCommandName, err)
		return 2
	}
	keycloak, missing, err := keycloakFromEnv(getenv)
	if len(missing) > 0 {
		fmt.Fprintf(errOut, "%s needs %s: a person's Personal e-mail is only in Keycloak\n", consentWithdrawCommandName, strings.Join(missing, ", "))
		return 2
	}
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", consentWithdrawCommandName, err)
		return 2
	}
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		fmt.Fprintf(errOut, "%s needs DATABASE_URL\n", consentWithdrawCommandName)
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(errOut, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	dir := identity.NewKeycloak(keycloak)
	svc := consent.NewService(pool, config, nil)
	svc.SetAddresses(account.NewPersonAddresses(identity.NewAccountIdentity(dir), user.NewPostgresStore(pool)))
	svc.SetPersonalEmailHolders(dir)
	return consentWithdrawCommand(ctx, in, out, errOut, time.Now().UTC(), purposes, svc.WithdrawByStaff)
}

// parseConsentWithdrawPurposes reads -purpose: one or more purposes core
// takes, comma-separated, each once.
func parseConsentWithdrawPurposes(args []string, errOut io.Writer) ([]consent.Purpose, int) {
	flags := flag.NewFlagSet(consentWithdrawCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	raw := flags.String("purpose", "", "the purposes to withdraw, comma-separated: promotional, event_invitations")
	if err := flags.Parse(args); err != nil {
		return nil, 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "%s reads the address from standard input, never from an argument\n", consentWithdrawCommandName)
		return nil, 2
	}
	var purposes []consent.Purpose
	for _, part := range strings.Split(*raw, ",") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		purpose, err := consent.ParsePurpose(part)
		if err != nil {
			fmt.Fprintf(errOut, "-purpose %q: not a purpose core takes (promotional, event_invitations)\n", strings.TrimSpace(part))
			return nil, 2
		}
		for _, have := range purposes {
			if have == purpose {
				fmt.Fprintf(errOut, "-purpose names %s twice\n", purpose)
				return nil, 2
			}
		}
		purposes = append(purposes, purpose)
	}
	if len(purposes) == 0 {
		fmt.Fprintln(errOut, "-purpose is required: promotional, event_invitations or both, comma-separated")
		return nil, 2
	}
	return purposes, 0
}

// consentWithdrawCommand reads the one address on in (surrounding space and
// a CRLF's CR trimmed, blank lines skipped), withdraws, and prints the
// record: the time and how many grants ended per purpose. Neither out nor
// errOut ever carries the address.
func consentWithdrawCommand(ctx context.Context, in io.Reader, out, errOut io.Writer, now time.Time, purposes []consent.Purpose,
	withdraw func(context.Context, []consent.Purpose, string) (map[consent.Purpose]int64, error)) int {
	address, err := consentWithdrawAddress(in)
	if err != nil {
		fmt.Fprintf(errOut, "standard input: %v\n", err)
		return 2
	}
	ended, err := withdraw(ctx, purposes, address)
	switch {
	case errors.Is(err, consent.ErrDisabled):
		fmt.Fprintf(errOut, "contact consents are off (%s is not set): core records none, so none is open\n", consent.KeyEnv)
		return consentWithdrawExitOff
	case errors.Is(err, consent.ErrInvalid):
		fmt.Fprintln(errOut, "standard input: not an e-mail address")
		return 2
	case errors.Is(err, consent.ErrAddressesUnavailable):
		// The error names what failed (Keycloak's status), never the person.
		fmt.Fprintf(errOut, "keycloak: %v; nothing ended, run it again\n", err)
		return 1
	case err != nil:
		fmt.Fprintf(errOut, "core: %v\n", err)
		writeConsentWithdrawRecord(out, now, purposes, ended)
		fmt.Fprintln(errOut, "not every purpose was done; run it again (what ended stays ended)")
		return 1
	}
	writeConsentWithdrawRecord(out, now, purposes, ended)
	return 0
}

func writeConsentWithdrawRecord(out io.Writer, now time.Time, purposes []consent.Purpose, ended map[consent.Purpose]int64) {
	fmt.Fprintln(out, "contact consent withdrawal by staff (ended_via staff)")
	fmt.Fprintf(out, "at: %s\n", now.Format(time.RFC3339))
	for _, purpose := range purposes {
		n, done := ended[purpose]
		if !done {
			fmt.Fprintf(out, "%s: not done\n", purpose)
			continue
		}
		fmt.Fprintf(out, "%s: %d ended\n", purpose, n)
	}
}

// consentWithdrawAddress is the one address on in. Two addresses are
// refused: a withdrawal is one person's request.
func consentWithdrawAddress(in io.Reader) (string, error) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 1024), 64<<10)
	var address string
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch {
		case line == "":
		case address != "":
			return "", errors.New("more than one line; give one address")
		default:
			address = line
		}
	}
	if err := scanner.Err(); err != nil {
		return "", errors.New("could not be read")
	}
	if address == "" {
		return "", errors.New("no address")
	}
	return address, nil
}
