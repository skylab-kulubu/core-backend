package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

// The operator's address lookup (docs/media-lifecycle.md, Address lookup):
// for a stage 5 migration that does not know who uploaded a stored
// address's Media, so cannot ask POST /v1/media/lookup for that person.
// Yusuf runs it inside the core container and hands the rows to the
// product. Rows go to standard output; counts and errors to standard error.
//
//	core-backend media-lookup [-product forms|cms] < addresses.txt > lookup.tsv
const mediaLookupCommandName = "media-lookup"

// mediaLookupRow is the header of the rows: no file name and no person,
// only whether the Media has an uploader a product can attach it for.
const mediaLookupRow = "address\tmedia_id\tpurpose\tstatus\tuploader"

// runMediaLookup wires the lookup to the database and the configured base.
func runMediaLookup(args []string, getenv func(string) string, in io.Reader, out, errOut io.Writer) int {
	flags := flag.NewFlagSet(mediaLookupCommandName, flag.ContinueOnError)
	flags.SetOutput(errOut)
	product := flags.String("product", "", "forms or cms: only the Media that product may link for their uploader, or holds")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(errOut, "%s reads the addresses from standard input, one a line\n", mediaLookupCommandName)
		return 2
	}
	if p := authz.Product(*product); p != "" && p != authz.ProductForms && p != authz.ProductCMS {
		fmt.Fprintln(errOut, "-product is forms or cms")
		return 2
	}
	ctx, stop := commandContext()
	defer stop()
	store, closeStore, code := legacyMediaStore(ctx, mediaLookupCommandName, getenv, errOut)
	if store == nil {
		return code
	}
	defer closeStore()
	base := media.PublicBaseFromEnv(getenv)
	if base == "" {
		base = media.DefaultPublicBase
	}
	return mediaLookupCommand(ctx, in, out, errOut, authz.Product(*product),
		func(ctx context.Context, addresses []string) ([]media.OperatorMatch, error) {
			return media.LookUpForOperator(ctx, store, base, authz.Product(*product), addresses)
		}, store.CountFullAddressKeys)
}

// mediaLookupCommand reads the addresses (one a line, blank lines skipped),
// writes one row per address in their order, and the counts.
func mediaLookupCommand(ctx context.Context, in io.Reader, out, errOut io.Writer, product authz.Product,
	lookUp func(context.Context, []string) ([]media.OperatorMatch, error), fullAddressKeys func(context.Context) (int, int, error)) int {
	addresses, err := lookupAddresses(in)
	if err != nil {
		fmt.Fprintf(errOut, "standard input: %v\n", err)
		return 2
	}
	matches, err := lookUp(ctx, addresses)
	if err != nil {
		fmt.Fprintf(errOut, "core: %v\n", err)
		return 1
	}
	all, current, err := fullAddressKeys(ctx)
	if err != nil {
		fmt.Fprintf(errOut, "core: %v\n", err)
		return 1
	}
	fmt.Fprintln(out, mediaLookupRow)
	named, unrecognised := 0, 0
	for _, match := range matches {
		// A tab would start another column.
		address := strings.NewReplacer("\t", " ", "\n", " ").Replace(match.Address)
		if match.MediaID == nil {
			if !match.Recognised {
				unrecognised++
			}
			fmt.Fprintf(out, "%s\t-\t-\t-\t-\n", address)
			continue
		}
		named++
		uploader := "n"
		if match.HasUploader {
			uploader = "y"
		}
		fmt.Fprintf(out, "%s\t%s\t%s\t%s\t%s\n", address, match.MediaID, match.Purpose, match.Status, uploader)
	}
	if product == "" {
		fmt.Fprintln(errOut, "Media address lookup: every Media")
	} else {
		fmt.Fprintf(errOut, "Media address lookup for product: %s (what it may link for the uploader, or holds)\n", product)
	}
	fmt.Fprintf(errOut, "addresses: %d\n", len(matches))
	fmt.Fprintf(errOut, "named a Media: %d\n", named)
	fmt.Fprintf(errOut, "named none: %d (not a core address: %d)\n", len(matches)-named, unrecognised)
	fmt.Fprintf(errOut, "Media stored with a full address, which no lookup finds: %d (%d current)\n", all, current)
	return 0
}

// lookupAddresses are the addresses on in, one a line: surrounding space
// (a CRLF's CR too) trimmed, blank lines skipped.
func lookupAddresses(in io.Reader) ([]string, error) {
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	addresses := []string{}
	for scanner.Scan() {
		if address := strings.TrimSpace(scanner.Text()); address != "" {
			addresses = append(addresses, address)
		}
	}
	return addresses, scanner.Err()
}
