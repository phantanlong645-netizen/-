package model

import "time"

type OrganizationTag struct {
	TagID       string    `gorm:"type:varchar(255);primaryKey" json:"tagId"`
	Name        string    `gorm:"type:varchar(100);not null" json:"name"`
	Description string    `gorm:"type:text" json:"description"`
	ParentTag   *string   `gorm:"type:varchar(255)" json:"parentTag"`
	CreatedBy   uint      `gorm:"not null" json:"createdBy"`
	CreatedAt   time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

type OrganizationTagNode struct {
	TagID       string                 `json:"tagId"`
	Name        string                 `json:"name"`
	Description string                 `json:"description"`
	ParentTag   *string                `json:"parentTag"`
	Children    []*OrganizationTagNode `json:"children"`
}

func (OrganizationTag) TableName() string {
	return "organization_tags"
}
