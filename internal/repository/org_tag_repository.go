package repository

import (
	"RAG-repository/internal/model"

	"gorm.io/gorm"
)

// OrgTagRepository 定义组织标签的数据访问能力。
type OrgTagRepository interface {
	Create(tag *model.OrganizationTag) error
	FindByID(id string) (*model.OrganizationTag, error)
	FindAll() ([]model.OrganizationTag, error)
	FindBatchByIDs(ids []string) ([]model.OrganizationTag, error)
	Update(tag *model.OrganizationTag) error
	Delete(id string) error
}

type orgTagRepository struct {
	db *gorm.DB
}

// NewOrgTagRepository 创建组织标签仓储实例。
func NewOrgTagRepository(db *gorm.DB) OrgTagRepository {
	return &orgTagRepository{db: db}
}

// Create 创建组织标签。
func (r *orgTagRepository) Create(tag *model.OrganizationTag) error {
	return r.db.Create(tag).Error
}

// FindAll 查询所有组织标签。
func (r *orgTagRepository) FindAll() ([]model.OrganizationTag, error) {
	var tags []model.OrganizationTag

	err := r.db.Find(&tags).Error
	return tags, err
}

// FindBatchByIDs 根据多个 tag_id 批量查询组织标签。
func (r *orgTagRepository) FindBatchByIDs(ids []string) ([]model.OrganizationTag, error) {
	var tags []model.OrganizationTag

	if len(ids) == 0 {
		return tags, nil
	}

	err := r.db.Where("tag_id IN ?", ids).Find(&tags).Error
	return tags, err
}

// FindByID 根据 tag_id 查询组织标签。
func (r *orgTagRepository) FindByID(tagID string) (*model.OrganizationTag, error) {
	var tag model.OrganizationTag

	err := r.db.Where("tag_id = ?", tagID).First(&tag).Error
	if err != nil {
		return nil, err
	}

	return &tag, nil
}

// Update 更新组织标签。
func (r *orgTagRepository) Update(tag *model.OrganizationTag) error {
	return r.db.Save(tag).Error
}

// Delete 根据 tag_id 删除组织标签。
func (r *orgTagRepository) Delete(tagID string) error {
	return r.db.Delete(&model.OrganizationTag{}, "tag_id = ?", tagID).Error
}
