package repository

import (
	"RAG-repository/internal/model"

	"gorm.io/gorm"
)

type DocumentVectorRepository interface {
	BatchCreate(vectors []*model.DocumentVector) error
	FindByFileMD5(fileMD5 string) ([]*model.DocumentVector, error)
	DeleteByFileMD5(fileMD5 string) error
}

type documentVectorRepository struct {
	db *gorm.DB
}

func NewDocumentVectorRepository(db *gorm.DB) DocumentVectorRepository {
	return &documentVectorRepository{db: db}
}

func (r *documentVectorRepository) BatchCreate(vectors []*model.DocumentVector) error {
	if len(vectors) == 0 {
		return nil
	}

	return r.db.CreateInBatches(vectors, 100).Error
}

func (r *documentVectorRepository) FindByFileMD5(fileMD5 string) ([]*model.DocumentVector, error) {
	var vectors []*model.DocumentVector

	err := r.db.Where("file_md5 = ?", fileMD5).Find(&vectors).Error
	return vectors, err
}

func (r *documentVectorRepository) DeleteByFileMD5(fileMD5 string) error {
	return r.db.Where("file_md5 = ?", fileMD5).Delete(&model.DocumentVector{}).Error
}
