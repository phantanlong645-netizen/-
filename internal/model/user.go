package model

import "time"

type User struct {
	ID         uint      `gorm:"primaryKey;autoIncrement" json:"id"`
	Username   string    `gorm:"type:varchar(255);not null;unique" json:"username"`
	Password   string    `gorm:"type:varchar(255);not null" json:"-"`
	Role       string    `gorm:"type:enum('USER', 'ADMIN');default:'USER'" json:"role"`
	OrgTags    string    `gorm:"type:varchar(255)" json:"orgTags"`
	PrimaryOrg string    `gorm:"type:varchar(50)" json:"primaryOrg"`
	CreatedAt  time.Time `gorm:"autoCreateTime" json:"createdAt"`
	UpdatedAt  time.Time `gorm:"autoUpdateTime" json:"updatedAt"`
}

func (User) TableName() string {
	return "users"
}
