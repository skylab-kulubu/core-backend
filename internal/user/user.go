package user

import (
	"time"

	"github.com/google/uuid"
)

type AccountState string

const (
	AccountActive          AccountState = "active"
	AccountDeletionPending AccountState = "deletion_pending"
	AccountAnonymized      AccountState = "anonymized"
)

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
	ID                  uuid.UUID    `json:"id"`
	Email               string       `json:"email"`
	FirstName           string       `json:"firstName"`
	LastName            string       `json:"lastName"`
	Username            string       `json:"username,omitempty"`
	SchoolEmail         string       `json:"schoolEmail,omitempty"`
	SkyNumber           string       `json:"skyNumber,omitempty"`
	StudentCardUID      string       `json:"-"`
	StudentCardLinked   bool         `json:"studentCardLinked"`
	Linkedin            string       `json:"linkedin,omitempty"`
	University          string       `json:"university,omitempty"`
	Faculty             string       `json:"faculty,omitempty"`
	Department          string       `json:"department,omitempty"`
	Phone               string       `json:"-"`
	ProfilePictureID    *uuid.UUID   `json:"profilePictureId,omitempty"`
	ProfilePictureURL   string       `json:"profilePictureUrl,omitempty"`
	AccountState        AccountState `json:"-"`
	DeletionRequestedAt *time.Time   `json:"-"`
	AnonymizedAt        *time.Time   `json:"-"`
	CreatedAt           time.Time    `json:"createdAt"`
	UpdatedAt           time.Time    `json:"updatedAt"`
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
	// claims. They are empty when the token carries no YTÜ claims; see
	// ytu.FromClaims for what they mean and how they are cleaned.
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
