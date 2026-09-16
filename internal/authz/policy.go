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
)

type Type string

const (
	TypeEvent      Type = "EVENT"
	TypeGroup      Type = "GROUP"
	TypeUser       Type = "USER"
	TypeTeam       Type = "TEAM"
	TypeTicket     Type = "TICKET"
	TypeSeason     Type = "SEASON"
	TypeEventDay   Type = "EVENT_DAY"
	TypeSession    Type = "SESSION"
	TypeCompetitor Type = "COMPETITOR"
	TypeMedia      Type = "MEDIA"
	TypeURL        Type = "URL"
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
	Type      Type
	OwnerTeam string
	EventType string
	OwnerID   string
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
