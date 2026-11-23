package identity

import (
	"context"
	"fmt"
	"strings"
	"time"
	_ "time/tzdata" // "new" is the Istanbul month whatever the image carries.

	"github.com/skylab-kulubu/core-backend/internal/authz"
	"github.com/skylab-kulubu/core-backend/internal/user"
)

// The user directory's page size (GET /v1/users/directory): the default
// and the most a page holds.
const (
	DirectoryDefaultSize = 50
	DirectoryMaxSize     = 100
	// DirectoryMaxPage is the last page a read may ask for: a page past it
	// would carry the offset past what an int and Postgres' OFFSET hold.
	DirectoryMaxPage = 1_000_000
)

// ErrDirectoryPage is a directory page past DirectoryMaxPage (400
// page_invalid); an ErrInvalid.
var ErrDirectoryPage = fmt.Errorf("identity: directory page past %d: %w", DirectoryMaxPage, ErrInvalid)

// The user directory's filters.
const (
	// DirectoryFilterNoSchoolEmail keeps the people with no school e-mail.
	DirectoryFilterNoSchoolEmail = "no_school_email"
	// DirectoryFilterNew keeps the people whose row was created this
	// month, in Europe/Istanbul.
	DirectoryFilterNew = "new"
)

// directoryTimeZone is the zone "this month" of DirectoryFilterNew is read
// in.
const directoryTimeZone = "Europe/Istanbul"

// UserDirectoryQuery is a read of the user directory. Page counts from 1;
// zero Size is DirectoryDefaultSize, and a Size above DirectoryMaxSize is
// DirectoryMaxSize.
type UserDirectoryQuery struct {
	Query  string
	Filter string
	Page   int
	Size   int
	// Malformed is a page or size the route could not read: 400 once the
	// caller may read users.
	Malformed bool
}

// DirectoryPerson is a user directory entry: the person as GET /v1/users
// answers them, and when core's row was created (for a caller who reads
// people whole).
type DirectoryPerson struct {
	Person
	CreatedAt *time.Time `json:"createdAt,omitempty"`
}

// UserDirectoryPage is a page of the user directory and how many people
// match in all.
type UserDirectoryPage struct {
	Items []DirectoryPerson `json:"items"`
	Total int               `json:"total"`
	Page  int               `json:"page"`
	Size  int               `json:"size"`
}

// UserDirectory is a page of the active people core holds a row for, newest
// first (GET /v1/users/directory, docs/user-accounts.md "The user list").
// It reads core's users table alone, never Keycloak. Who may read it and
// what they see is GET /v1/users's rule: users:read (the User Read
// decision), else 403, with no seat search; a caller without ReadFull gets
// the limited entry (projectPeople), searches the e-mail and the names
// alone (safePeopleSearch), sees no createdAt, and may not filter on what
// that entry leaves out (403).
func (s *service) UserDirectory(ctx context.Context, p authz.Principal, in UserDirectoryQuery) (UserDirectoryPage, error) {
	if err := s.allow(p, authz.TypeUser, authz.Read); err != nil {
		return UserDirectoryPage{}, err
	}
	if in.Malformed || in.Page < 1 || in.Size < 0 {
		return UserDirectoryPage{}, ErrInvalid
	}
	if in.Page > DirectoryMaxPage {
		return UserDirectoryPage{}, ErrDirectoryPage
	}
	size := in.Size
	if size == 0 {
		size = DirectoryDefaultSize
	}
	size = min(size, DirectoryMaxSize)
	full := s.authz.Allow(p, authz.Resource{Type: authz.TypeUser}, authz.ReadFull)
	q := user.DirectoryQuery{
		Query:     strings.TrimSpace(in.Query),
		NamesOnly: !full,
		Offset:    (in.Page - 1) * size,
		Limit:     size,
	}
	switch strings.TrimSpace(in.Filter) {
	case "":
	case DirectoryFilterNoSchoolEmail:
		q.NoSchoolEmail = true
	case DirectoryFilterNew:
		since, err := monthStart(s.now(), directoryTimeZone)
		if err != nil {
			return UserDirectoryPage{}, err
		}
		q.CreatedSince = since
	default:
		return UserDirectoryPage{}, ErrInvalid
	}
	if !full && (q.NoSchoolEmail || !q.CreatedSince.IsZero()) {
		// The limited entry carries neither the school e-mail nor the
		// creation time; filtering on them would tell them.
		return UserDirectoryPage{}, ErrForbidden
	}
	rows, err := s.users.ListDirectory(ctx, q)
	if err != nil {
		return UserDirectoryPage{}, err
	}
	people := make([]Person, 0, len(rows.Users))
	for _, u := range rows.Users {
		people = append(people, personFromUser(u))
	}
	people = projectPeople(people, full)
	page := UserDirectoryPage{Items: make([]DirectoryPerson, 0, len(people)), Total: rows.Total, Page: in.Page, Size: size}
	for i, person := range people {
		entry := DirectoryPerson{Person: person}
		if full {
			created := rows.Users[i].CreatedAt.UTC()
			entry.CreatedAt = &created
		}
		page.Items = append(page.Items, entry)
	}
	return page, nil
}

// monthStart is the first instant of now's month in the zone.
func monthStart(now time.Time, zone string) (time.Time, error) {
	loc, err := time.LoadLocation(zone)
	if err != nil {
		return time.Time{}, err
	}
	local := now.In(loc)
	return time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc), nil
}
