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
	TypeFormLink            Type = "FORM_LINK"
	TypeCertificate         Type = "CERTIFICATE"
	TypeCertificateTemplate Type = "CERTIFICATE_TEMPLATE"
	// TypeMediaAttachment is a Media attachment another product makes or
	// removes for its own records through the service attach API.
	TypeMediaAttachment Type = "MEDIA_ATTACHMENT"
	// TypeMediaReadLink is a five-minute read link to a private Media that
	// its owning product asks core for.
	TypeMediaReadLink Type = "MEDIA_READ_LINK"
	// TypeGithubActivity is the club's GitHub organisation activity on the
	// admin dashboard, private repositories' totals included.
	TypeGithubActivity Type = "GITHUB_ACTIVITY"
	// TypeFormResponse is an answer to a Skyforms form that the forms
	// service reports, so that core writes the Ticket it earns.
	TypeFormResponse Type = "FORM_RESPONSE"
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
	// Product is the product whose service account the caller is (see
	// ServiceClients). Empty for a person, whatever client their token was
	// issued to.
	Product Product
	// Client is the Keycloak client the token was issued to (azp). No
	// decision reads it; it only labels the role mode's disagreement count.
	Client string
	// ServiceAccount is true for a client's service account token, never a
	// person. A service account is never Privileged through a role of the
	// contract (roles.go), whatever roles it holds.
	ServiceAccount bool
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
	// MediaOwner is the product that owns the Media purpose (its
	// OwningProduct), for Upload on TypeMedia: the one product whose service
	// account may upload a MediaUploaderServiceOnly purpose.
	MediaOwner Product
}

type Policy struct {
	// PrivilegedGroups are the Groups whose members, and their subgroups'
	// members, are Privileged. RoleMode says whether a decision still reads
	// them.
	PrivilegedGroups []string
	// RoleMode is where Privileged decisions come from: the Groups, the
	// roles that stand for them (roles.go), or either. Empty is groups.
	RoleMode         RoleMode
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
// catalogue names one for every purpose; every rule needs a signed-in caller.
type MediaUploader string

const (
	// MediaUploaderAuthenticated is any signed-in person.
	MediaUploaderAuthenticated MediaUploader = "authenticated"
	// MediaUploaderEventEditor is a person who may create an Event for at
	// least one Owner team: the Event create decision, _default fallback
	// included. Which Event the Media ends up on is checked when it is linked.
	MediaUploaderEventEditor MediaUploader = "event_editor"
	// MediaUploaderCertificateTemplateEditor is a person who may create a
	// certificate template for at least one Owner team: the certificate
	// template create decision.
	MediaUploaderCertificateTemplateEditor MediaUploader = "certificate_template_editor"
	// MediaUploaderServiceOnly is no person: only the owning product's service
	// account, with the media:attach role, may start such an upload (Skyforms
	// for a guest Answer file). Core's own purposes (a video's frame) are
	// uploaded by no caller: core stores them itself.
	MediaUploaderServiceOnly MediaUploader = "service_only"
)

// Known reports whether u is a rule the authorizer decides.
func (u MediaUploader) Known() bool {
	_, ok := mediaUploaders[u]
	return ok
}
