package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/Nerzal/gocloak/v13"
)

func main() {
	apply := flag.Bool("apply", false, "unlink federated LDAP identities")
	flag.Parse()

	base := strings.TrimRight(os.Getenv("KEYCLOAK_URL"), "/")
	realm := os.Getenv("KEYCLOAK_REALM")
	clientID := os.Getenv("KEYCLOAK_CLIENT_ID")
	secret := os.Getenv("KEYCLOAK_CLIENT_SECRET")
	if parts := strings.SplitN(base, "/realms/", 2); len(parts) == 2 {
		base = parts[0]
		if realm == "" {
			realm = parts[1]
		}
	}
	if base == "" || realm == "" || clientID == "" || secret == "" {
		log.Fatal("KEYCLOAK_URL, KEYCLOAK_REALM, KEYCLOAK_CLIENT_ID, KEYCLOAK_CLIENT_SECRET are required")
	}

	ctx := context.Background()
	gc := gocloak.NewClient(base)
	jwt, err := gc.LoginClient(ctx, clientID, secret, realm)
	if err != nil {
		log.Fatal(err)
	}

	first := 0
	const pageSize = 100
	found := 0
	unlinked := 0
	for {
		users, err := gc.GetUsers(ctx, jwt.AccessToken, realm, gocloak.GetUsersParams{
			First: gocloak.IntP(first),
			Max:   gocloak.IntP(pageSize),
		})
		if err != nil {
			log.Fatal(err)
		}
		for _, u := range users {
			if u == nil || u.ID == nil {
				continue
			}
			links, err := gc.GetUserFederatedIdentities(ctx, jwt.AccessToken, realm, *u.ID)
			if err != nil {
				log.Fatal(err)
			}
			federated := gocloak.PString(u.FederationLink) != "" || len(links) > 0
			if !federated {
				continue
			}
			found++
			fmt.Printf("%s\t%s\t%s\n", *u.ID, gocloak.PString(u.Email), gocloak.PString(u.FederationLink))
			if !*apply {
				continue
			}
			for _, link := range links {
				if link == nil || link.IdentityProvider == nil {
					continue
				}
				if err := gc.DeleteUserFederatedIdentity(ctx, jwt.AccessToken, realm, *u.ID, *link.IdentityProvider); err != nil {
					log.Fatal(err)
				}
			}
			unlinked++
		}
		if len(users) < pageSize {
			break
		}
		first += len(users)
	}
	if *apply {
		fmt.Printf("unlinked %d of %d federated users\n", unlinked, found)
		return
	}
	fmt.Printf("dry-run %d federated users; pass -apply to unlink\n", found)
}
