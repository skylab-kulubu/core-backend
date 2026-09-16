package identity

type LocalizedText struct {
	TR string `json:"tr"`
	EN string `json:"en,omitempty"`
}

type PublicMember struct {
	FirstName         string `json:"firstName"`
	LastName          string `json:"lastName"`
	ProfilePictureURL string `json:"profilePictureUrl,omitempty"`
	Linkedin          string `json:"linkedin,omitempty"`
	University        string `json:"university,omitempty"`
	Faculty           string `json:"faculty,omitempty"`
	Department        string `json:"department,omitempty"`
	Leader            bool   `json:"leader"`
}

type PublicTeam struct {
	Team        string        `json:"team"`
	Path        string        `json:"path"`
	DisplayName LocalizedText `json:"displayName"`
}

type Roster struct {
	Team        string         `json:"team"`
	DisplayName LocalizedText  `json:"displayName"`
	Description *LocalizedText `json:"description,omitempty"`
	Count       int            `json:"count"`
	Members     []PublicMember `json:"members"`
}
