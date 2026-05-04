package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"

	"gorm.io/gorm"
)

type UserListResponse struct {
	Content       []UserDetailResponse `json:"content"`
	TotalElements int64                `json:"totalElements"`
	TotalPages    int                  `json:"totalPages"`
	Size          int                  `json:"size"`
	Number        int                  `json:"number"`
}

type UserDetailResponse struct {
	UserID     uint            `json:"userId"`
	Username   string          `json:"username"`
	Role       string          `json:"role"`
	OrgTags    []OrgTagDetail  `json:"orgTags"`
	PrimaryOrg string          `json:"primaryOrg"`
	Status     int             `json:"status"`
	CreatedAt  model.LocalTime `json:"createdAt"`
}

type OrgTagDetail struct {
	TagID string `json:"tagId"`
	Name  string `json:"name"`
}

type AdminService interface {
	CreateOrganizationTag(tagID, name, description, parentTag string, creator *model.User) (*model.OrganizationTag, error)
	ListOrganizationTags() ([]model.OrganizationTag, error)
	GetOrganizationTagTree() ([]*model.OrganizationTagNode, error)
	UpdateOrganizationTag(tagID string, name, description, parentTag string) (*model.OrganizationTag, error)
	DeleteOrganizationTag(tagID string) error

	AssignOrgTagsToUser(userID uint, orgTags []string) error
	ListUsers(page, size int) (*UserListResponse, error)
	GetAllConversations(ctx context.Context, userID *uint, startTime, endTime *time.Time) ([]map[string]interface{}, error)
}

type adminService struct {
	orgTagRepo       repository.OrgTagRepository
	userRepo         repository.UserRepository
	conversationRepo repository.ConversationRepository
}

func NewAdminService(
	orgTagRepo repository.OrgTagRepository,
	userRepo repository.UserRepository,
	conversationRepo repository.ConversationRepository,
) AdminService {
	return &adminService{
		orgTagRepo:       orgTagRepo,
		userRepo:         userRepo,
		conversationRepo: conversationRepo,
	}
}

func (s *adminService) CreateOrganizationTag(tagID, name, description, parentTag string, creator *model.User) (*model.OrganizationTag, error) {
	_, err := s.orgTagRepo.FindByID(tagID)
	if err == nil {
		return nil, errors.New("tagID 已存在")
	}

	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	tag := &model.OrganizationTag{
		TagID:       tagID,
		Name:        name,
		Description: description,
		CreatedBy:   creator.ID,
	}

	if parentTag != "" {
		tag.ParentTag = &parentTag
	}

	if err := s.orgTagRepo.Create(tag); err != nil {
		return nil, err
	}

	return tag, nil
}

func (s *adminService) GetOrganizationTagTree() ([]*model.OrganizationTagNode, error) {
	tags, err := s.orgTagRepo.FindAll()
	if err != nil {
		return nil, err
	}

	nodes := make(map[string]*model.OrganizationTagNode)
	tree := make([]*model.OrganizationTagNode, 0)

	for _, tag := range tags {
		nodes[tag.TagID] = &model.OrganizationTagNode{
			TagID:       tag.TagID,
			Name:        tag.Name,
			Description: tag.Description,
			ParentTag:   tag.ParentTag,
			Children:    []*model.OrganizationTagNode{},
		}
	}

	for _, node := range nodes {
		if node.ParentTag != nil && *node.ParentTag != "" {
			if parent, ok := nodes[*node.ParentTag]; ok {
				parent.Children = append(parent.Children, node)
			}
		} else {
			tree = append(tree, node)
		}
	}

	return tree, nil
}

func (s *adminService) ListOrganizationTags() ([]model.OrganizationTag, error) {
	return s.orgTagRepo.FindAll()
}

func (s *adminService) UpdateOrganizationTag(tagID string, name, description, parentTag string) (*model.OrganizationTag, error) {
	tag, err := s.orgTagRepo.FindByID(tagID)
	if err != nil {
		return nil, errors.New("tag not found")
	}

	tag.Name = name
	tag.Description = description

	if parentTag != "" {
		tag.ParentTag = &parentTag
	} else {
		tag.ParentTag = nil
	}

	if err := s.orgTagRepo.Update(tag); err != nil {
		return nil, err
	}

	return tag, nil
}

func (s *adminService) DeleteOrganizationTag(tagID string) error {
	return s.orgTagRepo.Delete(tagID)
}

func (s *adminService) AssignOrgTagsToUser(userID uint, orgTags []string) error {
	user, err := s.userRepo.FindByID(userID)
	if err != nil {
		return err
	}

	user.OrgTags = strings.Join(orgTags, ",")

	return s.userRepo.Update(user)
}

func (s *adminService) ListUsers(page, size int) (*UserListResponse, error) {
	offset := (page - 1) * size

	users, total, err := s.userRepo.FindWithPagination(offset, size)
	if err != nil {
		return nil, err
	}

	userResponses := make([]UserDetailResponse, 0, len(users))

	for _, user := range users {
		orgTagDetails := make([]OrgTagDetail, 0)

		if user.OrgTags != "" {
			tagIDs := strings.Split(user.OrgTags, ",")

			for _, tagID := range tagIDs {
				tag, err := s.orgTagRepo.FindByID(tagID)
				if err != nil {
					continue
				}

				orgTagDetails = append(orgTagDetails, OrgTagDetail{
					TagID: tag.TagID,
					Name:  tag.Name,
				})
			}
		}

		status := 1
		if user.Role == "ADMIN" {
			status = 0
		}

		userResponses = append(userResponses, UserDetailResponse{
			UserID:     user.ID,
			Username:   user.Username,
			Role:       user.Role,
			OrgTags:    orgTagDetails,
			PrimaryOrg: user.PrimaryOrg,
			Status:     status,
			CreatedAt:  model.LocalTime(user.CreatedAt),
		})
	}

	totalPages := 0
	if total > 0 && size > 0 {
		totalPages = (int(total) + size - 1) / size
	}

	return &UserListResponse{
		Content:       userResponses,
		TotalElements: total,
		TotalPages:    totalPages,
		Size:          size,
		Number:        page,
	}, nil
}

func (s *adminService) GetAllConversations(
	ctx context.Context,
	userID *uint,
	startTime, endTime *time.Time,
) ([]map[string]interface{}, error) {
	if userID != nil {
		user, err := s.userRepo.FindByID(*userID)
		if err != nil {
			return nil, errors.New("user not found")
		}

		return s.getConversationsForUser(ctx, user, startTime, endTime)
	}

	mappings, err := s.conversationRepo.GetAllUserConversationMappings(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get user conversation mappings from redis: %w", err)
	}

	allConversations := make([]map[string]interface{}, 0)

	for uid := range mappings {
		user, err := s.userRepo.FindByID(uid)
		if err != nil {
			continue
		}

		userConversations, err := s.getConversationsForUser(ctx, user, startTime, endTime)
		if err != nil {
			continue
		}

		allConversations = append(allConversations, userConversations...)
	}

	return allConversations, nil
}

func (s *adminService) getConversationsForUser(
	ctx context.Context,
	user *model.User,
	startTime, endTime *time.Time,
) ([]map[string]interface{}, error) {
	conversationID, err := s.conversationRepo.GetOrCreateConversationID(ctx, user.ID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) || err.Error() == "redis: nil" {
			return []map[string]interface{}{}, nil
		}

		return nil, fmt.Errorf("failed to get conversation id: %w", err)
	}

	history, err := s.conversationRepo.GetConversationHistory(ctx, conversationID)
	if err != nil {
		return nil, fmt.Errorf("failed to get conversation history: %w", err)
	}

	userConversations := make([]map[string]interface{}, 0)

	for _, msg := range history {
		if startTime != nil && msg.Timestamp.Before(*startTime) {
			continue
		}

		if endTime != nil && msg.Timestamp.After(*endTime) {
			continue
		}

		userConversations = append(userConversations, map[string]interface{}{
			"username":  user.Username,
			"role":      msg.Role,
			"content":   msg.Content,
			"timestamp": msg.Timestamp.Format("2006-01-02T15:04:05"),
		})
	}

	return userConversations, nil
}
