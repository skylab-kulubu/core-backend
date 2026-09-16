package authz

type Action string

const (
	Read   Action = "READ"
	Create Action = "CREATE"
	Update Action = "UPDATE"
	Delete Action = "DELETE"
)

type Type string

const (
	TypeEvent Type = "EVENT"
	TypeGroup Type = "GROUP"
	TypeUser  Type = "USER"
	TypeTeam  Type = "TEAM"
)

type Level string

const (
	LevelLeader Level = "LEADER"
	LevelMember Level = "MEMBER"
)

type Principal struct {
	ID     string
	Groups []string
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
