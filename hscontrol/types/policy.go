package types

import (
	"errors"

	"gorm.io/gorm"
)

var (
	ErrPolicyNotFound         = errors.New("acl policy not found")
	ErrPolicyUpdateIsDisabled = errors.New("update is disabled for modes other than 'database'")
)

// Policy represents a policy in the database.
type Policy struct {
	gorm.Model

	// TailnetID is the tenant this row belongs to (tailnet_id NOT NULL). The
	// tenantscope plugin sets and filters it; code never chooses it.
	TailnetID string `gorm:"column:tailnet_id;not null"`

	// Data contains the policy in HuJSON format.
	Data string
}
