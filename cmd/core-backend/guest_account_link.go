package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/skylab-kulubu/core-backend/internal/commpref"
	"github.com/skylab-kulubu/core-backend/internal/guestlink"
	"github.com/skylab-kulubu/core-backend/internal/identity"
	"github.com/skylab-kulubu/core-backend/internal/ticket"
)

// guestAccountLinkCommandName runs the one-time link of the guest Tickets
// and certificates there are to the accounts their addresses are, instead
// of the server (docs/guest-account-links.md):
//
//	core-backend guest-account-link          counts, writes nothing
//	core-backend guest-account-link -apply   turns linking on, then links them
//	core-backend guest-account-link -off     turns linking off; links stay
//	core-backend guest-account-link -undo    turns linking off, takes every link back
//
// It reads the server's environment, so it runs inside the core container.
// It prints counts only, never a person or an address.
const guestAccountLinkCommandName = "guest-account-link"

// guestAccountLinkTimeout bounds one run: the realm's directory (well under
// a second) and a write per row.
const guestAccountLinkTimeout = 10 * time.Minute

func runGuestAccountLink(args []string, getenv func(string) string, out io.Writer) int {
	var missing []string
	if strings.TrimSpace(getenv("DATABASE_URL")) == "" {
		missing = append(missing, "DATABASE_URL")
	}
	keycloakConfig, keycloakMissing, keycloakErr := keycloakFromEnv(getenv)
	missing = append(missing, keycloakMissing...)
	if len(missing) > 0 {
		fmt.Fprintf(out, "%s needs %s\n", guestAccountLinkCommandName, strings.Join(missing, ", "))
		return 2
	}
	if keycloakErr != nil {
		fmt.Fprintf(out, "%s: %v\n", guestAccountLinkCommandName, keycloakErr)
		return 2
	}
	alias, err := commpref.YTUIdentityProviderFromEnv(getenv)
	if err != nil {
		fmt.Fprintf(out, "%s: %v\n", guestAccountLinkCommandName, err)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), guestAccountLinkTimeout)
	defer cancel()
	pool, err := pgxpool.New(ctx, getenv("DATABASE_URL"))
	if err != nil {
		fmt.Fprintln(out, "database: cannot open the connection pool")
		return 1
	}
	defer pool.Close()
	keycloak := identity.NewKeycloak(keycloakConfig)
	read := func(ctx context.Context) (map[string]commpref.VerifiedAddress, error) {
		return commpref.ReadVerifiedAddresses(ctx, keycloak, alias)
	}
	return guestAccountLinkCommand(ctx, args, out, read, guestlink.NewPostgres(pool))
}

func guestAccountLinkCommand(ctx context.Context, args []string, out io.Writer, read func(context.Context) (map[string]commpref.VerifiedAddress, error), store *guestlink.Postgres) int {
	flags := flag.NewFlagSet(guestAccountLinkCommandName, flag.ContinueOnError)
	flags.SetOutput(out)
	apply := flags.Bool("apply", false, "turn linking on, then link the guest Tickets and certificates there are")
	off := flags.Bool("off", false, "turn linking off; the links made stay")
	undo := flags.Bool("undo", false, "turn linking off and take back every link core made")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	chosen := 0
	for _, f := range []bool{*apply, *off, *undo} {
		if f {
			chosen++
		}
	}
	if chosen > 1 || flags.NArg() > 0 {
		fmt.Fprintln(out, "use at most one of -apply, -off, -undo")
		return 2
	}
	switch {
	case *off:
		if err := store.SetEnabled(ctx, false); err != nil {
			fmt.Fprintf(out, "core: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, "guest account links: turned off")
		fmt.Fprintln(out, "linking: off")
		return 0
	case *undo:
		undone, err := store.Unlink(ctx)
		if err != nil {
			fmt.Fprintf(out, "core: %v\n", err)
			return 1
		}
		fmt.Fprintln(out, "guest account links: turned off and taken back")
		fmt.Fprintln(out, "linking: off")
		fmt.Fprintf(out, "tickets_unlinked: %d\n", undone.Tickets)
		fmt.Fprintf(out, "certificates_unlinked: %d\n", undone.Certificates)
		fmt.Fprintf(out, "certificates_issued_from_linked_unlinked: %d\n", undone.IssuedFromLinked)
		return 0
	}
	addrs, err := read(ctx)
	if err != nil {
		fmt.Fprintf(out, "keycloak: %v\n", err)
		return 1
	}
	if *apply {
		// On first: every link's write checks the switch (so a link
		// cannot outlive -off or -undo). A run that fails half way leaves
		// it on; running it again, or the user sync's next pass, links the
		// rest, and -undo takes all of it back.
		if err := store.SetEnabled(ctx, true); err != nil {
			fmt.Fprintf(out, "core: %v\n", err)
			return 1
		}
	}
	report, err := guestlink.Run(ctx, store, addrs, ticket.LinkBulk, *apply)
	if err != nil {
		fmt.Fprintf(out, "core: %v\n", err)
		return 1
	}
	if *apply {
		fmt.Fprintln(out, "guest account links: linking turned on, and linked")
	} else {
		fmt.Fprintln(out, "guest account links, dry run (add -apply to link and turn linking on)")
	}
	on, err := store.Enabled(ctx)
	if err != nil {
		fmt.Fprintf(out, "core: %v\n", err)
		return 1
	}
	state := "off"
	if on {
		state = "on"
	}
	fmt.Fprintf(out, "linking: %s\n", state)
	fmt.Fprintf(out, "verified_addresses: %d\n", len(addrs))
	printCounts(out, "tickets", report.Tickets, false)
	printCounts(out, "certificates", report.Certificates, true)
	linked, err := store.LinkedCounts(ctx)
	if err != nil {
		fmt.Fprintf(out, "core: %v\n", err)
		return 1
	}
	for _, kind := range []string{"tickets", "certificates"} {
		for _, src := range []ticket.LinkSource{ticket.LinkWrite, ticket.LinkSync, ticket.LinkBulk} {
			if kind == "certificates" && src == ticket.LinkWrite {
				continue
			}
			fmt.Fprintf(out, "%s_linked_so_far_%s: %d\n", kind, src, linked[kind+"_"+string(src)])
		}
	}
	return 0
}

// printCounts prints one kind's counts, one "name: n" a line, the wizard's
// format.
func printCounts(out io.Writer, kind string, c guestlink.Counts, certificates bool) {
	kinds := []guestlink.Kind{guestlink.KindPrimary, guestlink.KindSchool, guestlink.KindPersonal}
	if certificates {
		kinds = append([]guestlink.Kind{guestlink.KindTicket}, kinds...)
	}
	fmt.Fprintf(out, "%s_link: %d\n", kind, c.Total())
	for _, k := range kinds {
		fmt.Fprintf(out, "%s_link_%s: %d\n", kind, k, c.Linked[k])
	}
	fmt.Fprintf(out, "%s_ambiguous: %d\n", kind, c.Ambiguous)
	if !certificates {
		fmt.Fprintf(out, "%s_duplicate: %d\n", kind, c.Duplicate)
	}
	fmt.Fprintf(out, "%s_account_disabled: %d\n", kind, c.Disabled)
	fmt.Fprintf(out, "%s_account_inactive: %d\n", kind, c.Inactive)
	fmt.Fprintf(out, "%s_no_account: %d\n", kind, c.Unmatched)
	fmt.Fprintf(out, "%s_changed_meanwhile: %d\n", kind, c.Changed)
}
