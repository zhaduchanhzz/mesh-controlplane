package types

import (
	"database/sql"
	"time"
)

// TailnetStatus is the lifecycle state of a tailnet.
type TailnetStatus string

const (
	TailnetPending   TailnetStatus = "PENDING"
	TailnetActive    TailnetStatus = "ACTIVE"
	TailnetSuspended TailnetStatus = "SUSPENDED"
	TailnetDeleting  TailnetStatus = "DELETING"
)

// Tailnet is one tenant network. OwnerClusterID/OwnerEpoch fence which
// cluster may serve and mutate it (ADR 0005, I12). The tailnets table is not
// tenant-scoped by the tenantscope plugin (it has no tailnet_id column); only
// platform code reads it.
type Tailnet struct {
	ID             string        `gorm:"primaryKey"`
	Slug           string        `gorm:"not null"`
	Status         TailnetStatus `gorm:"not null"`
	OwnerClusterID string        `gorm:"column:owner_cluster_id;not null"`
	OwnerEpoch     uint64        `gorm:"column:owner_epoch;not null"`
	PolicyRevision uint64        `gorm:"column:policy_revision;not null"`
	ConfigRevision uint64        `gorm:"column:config_revision;not null"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
	DeletedAt      *time.Time
	// MovingToClusterID names the target cluster while the tailnet is being
	// moved (P12b); bootstrap refuses it then (P11). Nil otherwise.
	MovingToClusterID *string `gorm:"column:moving_to_cluster_id"`
}

// TailnetSettings is a tailnet's row of tailnet_settings (P06, ADR 0008
// §2), a tenant table: its DNS settings as validated JSON ([TenantDNS]),
// NULL for the defaults. Written by the operator (platform handle) and,
// since P12d, by the tenant through its runtime's scoped and fenced handle.
type TailnetSettings struct {
	// TailnetID is the tenant this row belongs to and its primary key; the
	// tenantscope plugin stamps and confines it.
	TailnetID string         `gorm:"column:tailnet_id;primaryKey"`
	DNSConfig sql.NullString `gorm:"column:dns_config"`
}

// TableName is the v0.1 table.
func (TailnetSettings) TableName() string { return "tailnet_settings" }
