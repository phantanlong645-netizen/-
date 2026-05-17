package model

import "time"

// 检索会话状态常量定义
const (
	ResearchSessionStatusCompleted = "COMPLETED" // 检索会话已完成
	ResearchSessionStatusFailed    = "FAILED"    // 检索会话失败
)

// 候选论文导入状态常量定义
const (
	ResearchCandidateImportPending  = "PENDING"  // 待导入
	ResearchCandidateImportImported = "IMPORTED" // 已导入
	ResearchCandidateImportFailed   = "FAILED"   // 导入失败
)

// ResearchSession 学术检索会话模型
// 记录用户发起的每一次学术论文检索请求的会话信息
type ResearchSession struct {
	ID             uint       `gorm:"primaryKey;autoIncrement" json:"id"`           // 会话ID，自增主键
	UserID         uint       `gorm:"not null;index" json:"userId"`                  // 用户ID，建立索引加速查询
	Query          string     `gorm:"type:varchar(1000);not null" json:"query"`      // 用户输入的检索问题
	PlannedQueries string     `gorm:"type:text" json:"plannedQueries"`               // LLM生成的多个检索query，JSON数组格式存储
	Status         string     `gorm:"type:varchar(32);not null" json:"status"`      // 会话状态：COMPLETED/FAILED
	ErrorMessage   string     `gorm:"type:varchar(1000)" json:"errorMessage"`        // 错误信息（当状态为FAILED时）
	CreatedAt      time.Time  `gorm:"autoCreateTime" json:"createdAt"`                // 创建时间，GORM自动管理
	CompletedAt    *time.Time `gorm:"default:null" json:"completedAt"`               // 完成时间，可为空（失败时可能为空）
}

// TableName 指定该模型对应数据库表名
func (ResearchSession) TableName() string {
	return "research_sessions"
}

// ResearchCandidate 学术检索候选论文模型
// 记录检索返回的每一篇候选论文的信息
type ResearchCandidate struct {
	ID              uint       `gorm:"primaryKey;autoIncrement" json:"id"`                            // 候选论文ID，自增主键
	SessionID       uint       `gorm:"not null;index" json:"sessionId"`                               // 所属会话ID，建立索引加速查询
	UserID          uint       `gorm:"not null;index" json:"userId"`                                  // 用户ID，建立索引加速查询
	Provider        string     `gorm:"type:varchar(50);not null;index:idx_research_candidate_source" json:"provider"`  // 论文来源（如Semantic Scholar、Arxiv）
	ExternalID      string     `gorm:"type:varchar(255);not null;index:idx_research_candidate_source" json:"externalId"` // 外部数据库中的论文ID
	Title           string     `gorm:"type:varchar(1000);not null" json:"title"`                      // 论文标题
	Abstract        string     `gorm:"type:text" json:"abstract"`                                     // 论文摘要
	Authors         string     `gorm:"type:text" json:"authors"`                                      // 作者列表，JSON数组格式存储
	Year            int        `json:"year"`                                                           // 发表年份
	URL             string     `gorm:"type:varchar(1000)" json:"url"`                                 // 论文主页URL
	PDFURL          string     `gorm:"type:varchar(1000)" json:"pdfUrl"`                               // PDF下载链接
	CitationCount   int        `json:"citationCount"`                                                 // 引用次数
	RelevanceScore  float64    `json:"relevanceScore"`                                                 // 相关性评分（综合算法和LLM评分）
	SelectionReason string     `gorm:"type:varchar(1000)" json:"selectionReason"`                     // 筛选原因说明
	ImportStatus    string     `gorm:"type:varchar(32);not null;default:'PENDING'" json:"importStatus"` // 导入状态：PENDING/IMPORTED/FAILED
	FileMD5         string     `gorm:"type:varchar(32);index" json:"fileMd5"`                         // 导入后文件的MD5值（导入成功后填充）
	ImportError     string     `gorm:"type:varchar(1000)" json:"importError"`                         // 导入错误信息（当导入失败时）
	CreatedAt       time.Time  `gorm:"autoCreateTime" json:"createdAt"`                               // 创建时间
	ImportedAt      *time.Time `gorm:"default:null" json:"importedAt"`                                // 导入成功时间
}

// TableName 指定该模型对应数据库表名
func (ResearchCandidate) TableName() string {
	return "research_candidates"
}
