package repository

import (
	"RAG-repository/internal/model"

	"gorm.io/gorm"
)

type UserRepository interface {
	Create(user *model.User) error
	FindByUsername(username string) (*model.User, error)
	Update(user *model.User) error
	FindAll() ([]model.User, error)
	FindWithPagination(offset, limit int) ([]model.User, int64, error)
	FindByID(userID uint) (*model.User, error)
}
type userRepository struct {
	db *gorm.DB
}

func NewUserRepository(db *gorm.DB) UserRepository {
	return &userRepository{
		db: db,
	}
}
func (userRepository *userRepository) Create(user *model.User) error {
	return userRepository.db.Create(user).Error
}

// FindByUsername 根据用户名查询用户。
func (r *userRepository) FindByUsername(username string) (*model.User, error) {
	var user model.User

	err := r.db.Where("username = ?", username).First(&user).Error
	if err != nil {
		return nil, err
	}

	return &user, nil
}

// Update 更新用户。
func (r *userRepository) Update(user *model.User) error {
	return r.db.Save(user).Error
}

// FindAll 查询所有用户。
func (r *userRepository) FindAll() ([]model.User, error) {
	var users []model.User

	err := r.db.Find(&users).Error
	return users, err
}

// FindWithPagination 分页查询用户。
func (r *userRepository) FindWithPagination(offset, limit int) ([]model.User, int64, error) {
	var users []model.User
	var total int64

	db := r.db.Model(&model.User{})

	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if err := db.Offset(offset).Limit(limit).Find(&users).Error; err != nil {
		return nil, 0, err
	}

	return users, total, nil
}

// FindByID 根据用户 ID 查询用户。
func (r *userRepository) FindByID(userID uint) (*model.User, error) {
	var user model.User

	err := r.db.First(&user, userID).Error
	if err != nil {
		return nil, err
	}

	return &user, nil
}
