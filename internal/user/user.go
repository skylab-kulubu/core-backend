package user

import (
	"time"

	"github.com/google/uuid"
	"github.com/skylab-kulubu/core-backend/internal/media"
)

type AccountState string

const (
	AccountActive          AccountState = "active"
	AccountDeletionPending AccountState = "deletion_pending"
	AccountAnonymized      AccountState = "anonymized"
)

// ReadStatus is the `status` a user read answers with: what an API client
// sees of the account state (docs/account-lifecycle.md#reading-a-person).
type ReadStatus string

const (
	ReadStatusActive ReadStatus = "active"
	// ReadStatusDeletionPending: the person asked to be erased and the
	// erasure is running. The read already hides their personal data, as
	// for ReadStatusDeleted; the request cannot be withdrawn.
	ReadStatusDeletionPending ReadStatus = "deletion_pending"
	// ReadStatusDeleted: the person is erased (anonymized or hard-purged),
	// or the id is DeletedSubject.
	ReadStatusDeleted ReadStatus = "deleted"
)

// DeletedSubject is the fixed sub the services put where an erased person
// was named, and DeletedDisplayName its name
// (docs/account-erasure-command.md §8). It is the same for everyone.
var DeletedSubject = uuid.MustParse("00000000-0000-4000-8000-000000000000")

const DeletedDisplayName = "Silinmiş kullanıcı"

// ReadStatus is the status a user read answers with for this state.
func (s AccountState) ReadStatus() ReadStatus {
	switch s {
	case AccountDeletionPending:
		return ReadStatusDeletionPending
	case AccountAnonymized:
		return ReadStatusDeleted
	default:
		return ReadStatusActive
	}
}

type DeletionRequestStatus string

type AttributionState string

const (
	AttributionAnonymous AttributionState = "anonymous"
	AttributionAllowed   AttributionState = "allowed"
	AttributionBlocked   AttributionState = "blocked"
)

type DeletionStep string

const (
	DeletionRequestPending            DeletionRequestStatus = "pending"
	DeletionRequestProcessing         DeletionRequestStatus = "processing"
	DeletionRequestCompleted          DeletionRequestStatus = "completed"
	DeletionRequestManualIntervention DeletionRequestStatus = "manual_intervention"

	DeletionStepDisableIdentity DeletionStep = "disable_identity"
	DeletionStepLogoutSessions  DeletionStep = "logout_sessions"
	DeletionStepAnonymizeCore   DeletionStep = "anonymize_core"
	DeletionStepEraseProfile    DeletionStep = "erase_profile_media"
	DeletionStepEraseUploads    DeletionStep = "erase_staged_uploads"
	DeletionStepDeleteIdentity  DeletionStep = "delete_identity"

	// Service erasure steps: one Erasure command each (ADR-0051). Their
	// checkpoint rows also keep the service's counts as completion proof.
	DeletionStepEraseSkyMail DeletionStep = "erase_skymail"
	DeletionStepEraseCMS     DeletionStep = "erase_cms"
	DeletionStepEraseForms   DeletionStep = "erase_forms"
)

type DeletionRequest struct {
	ID                uuid.UUID
	SubjectID         uuid.UUID
	RequestedBy       *uuid.UUID
	Status            DeletionRequestStatus
	AttemptCount      int
	NextAttemptAt     time.Time
	LeaseUntil        *time.Time
	LeaseToken        *uuid.UUID
	ProfileMediaID    *uuid.UUID
	LastErrorCode     string
	CreatedAt         time.Time
	UpdatedAt         time.Time
	CompletedAt       *time.Time
	PlatformBlockedAt *time.Time
}

type SelfDeletionIntake struct {
	IdempotencyHash   [32]byte
	ReceiptLookupHash [32]byte
	ReceiptHash       [32]byte
	ReceiptExpiresAt  time.Time
	CreatedAt         time.Time
}

type SelfDeletionRecord struct {
	Request DeletionRequest
	SelfDeletionIntake
	ReceiptRevokedAt *time.Time
	HasCompletedStep bool
}

type User struct {
	ID                uuid.UUID `json:"id"`
	Email             string    `json:"email"`
	FirstName         string    `json:"firstName"`
	LastName          string    `json:"lastName"`
	Username          string    `json:"username,omitempty"`
	SchoolEmail       string    `json:"schoolEmail,omitempty"`
	SkyNumber         string    `json:"skyNumber,omitempty"`
	StudentCardUID    string    `json:"-"`
	StudentCardLinked bool      `json:"studentCardLinked"`
	Linkedin          string    `json:"linkedin,omitempty"`
	University        string    `json:"university,omitempty"`
	Faculty           string    `json:"faculty,omitempty"`
	Department        string    `json:"department,omitempty"`
	// YTULinked is true once core has seen the YTÜ Microsoft login's
	// `university` claim for this person (see ytu.FromClaims). University,
	// Faculty and Department then follow that login and nobody edits them
	// through core. Only the caller's own view and the admin card show it.
	YTULinked           bool         `json:"-"`
	Phone               string       `json:"-"`
	ProfilePictureID    *uuid.UUID   `json:"profilePictureId,omitempty"`
	ProfilePictureURL   string       `json:"profilePictureUrl,omitempty"`
	AccountState        AccountState `json:"-"`
	DeletionRequestedAt *time.Time   `json:"-"`
	AnonymizedAt        *time.Time   `json:"-"`
	CreatedAt           time.Time    `json:"createdAt"`
	UpdatedAt           time.Time    `json:"updatedAt"`

	// ProfilePicture is the Media the profile links, as the store read it
	// with the profile: what the picture's size addresses are built from
	// (media.Addresses.LinkedSizes). Nil when the profile links none.
	ProfilePicture *media.LinkedImage `json:"-"`
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
	// University and Department are the raw `university` and `department`
	// claims. They are empty when the token carries no YTÜ claims (Account
	// Center's token never does); see ytu.FromClaims for what they mean and
	// how they are cleaned.
	University string
	Department string
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
