package model

import "time"

// ChatMessage 表示一条聊天消息。
// 这个结构主要用于 Redis 中的对话历史缓存。
type ChatMessage struct {
	Role      string    `json:"role"`
	Content   string    `json:"content"`
	Timestamp time.Time `json:"timestamp"`
}

// Conversation 表示一次完整问答。
// 后面 admin 或用户查看历史记录时会用到。
type Conversation struct {
	ID        uint      `gorm:"primaryKey" json:"id"`
	UserID    uint      `gorm:"index;not null" json:"userId"`
	Question  string    `gorm:"type:text;not null" json:"question"`
	Answer    string    `gorm:"type:text;not null" json:"answer"`
	CreatedAt time.Time `gorm:"autoCreateTime" json:"createdAt"`
}

func (Conversation) TableName() string {
	return "conversations"
}
