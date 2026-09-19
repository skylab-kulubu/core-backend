package user

import (
	"time"

	"github.com/google/uuid"
)

type User struct {
	ID                uuid.UUID  `json:"id"`
	Email             string     `json:"email"`
	FirstName         string     `json:"firstName"`
	LastName          string     `json:"lastName"`
	Username          string     `json:"username,omitempty"`
	SchoolEmail       string     `json:"schoolEmail,omitempty"`
	SkyNumber         string     `json:"skyNumber,omitempty"`
	StudentCardUID    string     `json:"-"`
	StudentCardLinked bool       `json:"studentCardLinked"`
	Linkedin          string     `json:"linkedin,omitempty"`
	University        string     `json:"university,omitempty"`
	Faculty           string     `json:"faculty,omitempty"`
	Department        string     `json:"department,omitempty"`
	Phone             string     `json:"-"`
	ProfilePictureID  *uuid.UUID `json:"profilePictureId,omitempty"`
	ProfilePictureURL string     `json:"profilePictureUrl,omitempty"`
	CreatedAt         time.Time  `json:"createdAt"`
	UpdatedAt         time.Time  `json:"updatedAt"`
}

func withStudentCardStatus(u User) User {
	u.StudentCardLinked = u.StudentCardUID != ""
	return u
}

type Profile struct {
	Email       string
	FirstName   string
	LastName    string
	Username    string
	SchoolEmail string
	SkyNumber   string
}

type ProfileUpdate struct {
	FirstName  string
	LastName   string
	Linkedin   string
	University string
	Faculty    string
	Department string
}

type ProfilePatch struct {
	FirstName  *string
	LastName   *string
	Linkedin   *string
	University *string
	Faculty    *string
	Department *string
	Phone      *string
}
