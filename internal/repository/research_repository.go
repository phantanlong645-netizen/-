package repository

import (
	"RAG-repository/internal/model"

	"gorm.io/gorm"
)

// ResearchRepository 学术检索相关的数据访问层接口
// 定义检索会话和候选论文的数据库操作方法
type ResearchRepository interface {
	// CreateSession 创建新的检索会话记录
	CreateSession(session *model.ResearchSession) error
	// UpdateSession 更新检索会话记录（如状态、错误信息、完成时间等）
	UpdateSession(session *model.ResearchSession) error
	// CreateCandidates 批量创建候选论文记录
	CreateCandidates(candidates []*model.ResearchCandidate) error
	// ListCandidates 查询指定会话下的所有候选论文列表，按相关性和引用量排序
	ListCandidates(sessionID uint, userID uint) ([]model.ResearchCandidate, error)
	// GetCandidate 根据ID获取单个候选论文详情
	GetCandidate(candidateID uint, userID uint) (*model.ResearchCandidate, error)
	// UpdateCandidate 更新候选论文记录（如导入状态、文件MD5等）
	UpdateCandidate(candidate *model.ResearchCandidate) error
}

// researchRepository 数据访问层实现结构体
type researchRepository struct {
	db *gorm.DB // GORM数据库实例
}

// NewResearchRepository 创建ResearchRepository实例
func NewResearchRepository(db *gorm.DB) ResearchRepository {
	return &researchRepository{db: db}
}

// CreateSession 创建检索会话记录
func (r *researchRepository) CreateSession(session *model.ResearchSession) error {
	return r.db.Create(session).Error
}

// UpdateSession 更新检索会话记录
func (r *researchRepository) UpdateSession(session *model.ResearchSession) error {
	return r.db.Save(session).Error
}

// CreateCandidates 批量创建候选论文记录
// 使用CreateInBatches进行批量插入以提高性能，每批100条
func (r *researchRepository) CreateCandidates(candidates []*model.ResearchCandidate) error {
	if len(candidates) == 0 {
		return nil
	}
	return r.db.CreateInBatches(candidates, 100).Error
}

// ListCandidates 查询指定会话下的候选论文列表
// 按相关性评分、引用量、年份降序排序
// 同时验证用户ID确保数据隔离
func (r *researchRepository) ListCandidates(sessionID uint, userID uint) ([]model.ResearchCandidate, error) {
	var candidates []model.ResearchCandidate
	err := r.db.
		Where("session_id = ? AND user_id = ?", sessionID, userID).
		Order("relevance_score desc, citation_count desc, year desc").
		Find(&candidates).
		Error
	return candidates, err
}

// GetCandidate 获取单个候选论文详情
// 同时验证用户ID确保数据隔离，只能获取属于自己的候选论文
func (r *researchRepository) GetCandidate(candidateID uint, userID uint) (*model.ResearchCandidate, error) {
	var candidate model.ResearchCandidate
	err := r.db.
		Where("id = ? AND user_id = ?", candidateID, userID).
		First(&candidate).
		Error
	if err != nil {
		return nil, err
	}
	return &candidate, nil
}

// UpdateCandidate 更新候选论文记录
func (r *researchRepository) UpdateCandidate(candidate *model.ResearchCandidate) error {
	return r.db.Save(candidate).Error
}
