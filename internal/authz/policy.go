package authz

type Action string

const (
	Read     Action = "READ"
	Create   Action = "CREATE"
	Update   Action = "UPDATE"
	Delete   Action = "DELETE"
	Validate Action = "VALIDATE"
	ReadMe   Action = "READ_ME"
	Upload   Action = "UPLOAD"
	List     Action = "LIST"
	Issue    Action = "ISSUE"
	Revoke   Action = "REVOKE"
	Assign   Action = "ASSIGN"
)

type Type string

const (
	TypeEvent               Type = "EVENT"
	TypeGroup               Type = "GROUP"
	TypeUser                Type = "USER"
	TypeTeam                Type = "TEAM"
	TypeTicket              Type = "TICKET"
	TypeSeason              Type = "SEASON"
	TypeEventDay            Type = "EVENT_DAY"
	TypeSession             Type = "SESSION"
	TypeCompetitor          Type = "COMPETITOR"
	TypeMedia               Type = "MEDIA"
	TypeURL                 Type = "URL"
	TypeCertificate         Type = "CERTIFICATE"
	TypeCertificateTemplate Type = "CERTIFICATE_TEMPLATE"
)

type Level string

const (
	LevelLeader Level = "LEADER"
	LevelMember Level = "MEMBER"
)

type Principal struct {
	ID     string
	Groups []string
	Roles  []string
}

type Resource struct {
	Type         Type
	OwnerTeam    string
	OwnerID      string
	DoorStaffIDs []string
	TeamDoorScan bool
	// MediaUploader is the upload rule of a Media purpose, for Upload on
	// TypeMedia. Empty is MediaUploaderAuthenticated.
	MediaUploader MediaUploader
}

type Policy struct {
	PrivilegedGroups []string
	LeaderSubgroups  []string
	EventPermissions map[string]map[Action][]Level
}

func DefaultPolicy() Policy {
	return Policy{
		PrivilegedGroups: []string{"ADMIN", "YK", "DK"},
		LeaderSubgroups:  []string{"LIDERLER", "KOORDINATORLER"},
		EventPermissions: map[string]map[Action][]Level{
			"_default": {
				Create: {LevelLeader},
				Update: {LevelLeader},
				Delete: {LevelLeader},
			},
			"GECEKODU": {
				Create: {LevelLeader, LevelMember},
				Update: {LevelLeader, LevelMember},
				Delete: {LevelLeader},
			},
		},
	}
}

// MediaUploader is who may upload Media of a Media purpose. The Media purpose
// catalogue names one for every purpose.
type MediaUploader string

const (
	// MediaUploaderAuthenticated is any signed-in person.
	MediaUploaderAuthenticated MediaUploader = "authenticated"
	// MediaUploaderEventEditor is a person who may create Events for at least
	// one Owner team: a privileged person, a leader or coordinator of any
	// team, or a member of a team whose Event permissions let members create
	// Events. Which Event a Media ends up on is checked when it is linked.
	MediaUploaderEventEditor MediaUploader = "event_editor"
	// MediaUploaderCertificateTemplate is a person who may create certificate
	// templates for at least one Owner team: a privileged person, a leader or
	// coordinator of any team, or a team member holding
	// certificate:template:manage.
	MediaUploaderCertificateTemplate MediaUploader = "certificate_template"
	// MediaUploaderServiceOnly is no person: only the owning product's service
	// identity may start such an upload.
	MediaUploaderServiceOnly MediaUploader = "service_only"
)

// Known reports whether u is one of the MediaUploader values above.
func (u MediaUploader) Known() bool {
	switch u {
	case MediaUploaderAuthenticated, MediaUploaderEventEditor, MediaUploaderCertificateTemplate, MediaUploaderServiceOnly:
		return true
	}
	return false
}
