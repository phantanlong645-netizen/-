package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/pkg/database"
	"RAG-repository/pkg/hash"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/token"

	"gorm.io/gorm"
)

type UserService interface {
	Register(username, password string) (*model.User, error)
	Login(username, password string) (accessToken, refreshToken string, err error)
	GetProfile(username string) (*model.User, error)
	Logout(tokenString string) error
	SetUserPrimaryOrg(username, orgTag string) error
	GetUserOrgTags(username string) (map[string]interface{}, error)
	GetUserEffectiveOrgTags(user *model.User) ([]string, error)
	RefreshToken(refreshTokenString string) (newAccessToken, newRefreshToken string, err error)
}

type userService struct {
	userRepo   repository.UserRepository
	orgTagRepo repository.OrgTagRepository
	jwtManager *token.JWTManager
}

func NewUserService(
	userRepo repository.UserRepository,
	orgTagRepo repository.OrgTagRepository,
	jwtManager *token.JWTManager,
) UserService {
	return &userService{
		userRepo:   userRepo,
		orgTagRepo: orgTagRepo,
		jwtManager: jwtManager,
	}
}

func (s *userService) Register(username, password string) (*model.User, error) {
	_, err := s.userRepo.FindByUsername(username)
	if err == nil {
		return nil, errors.New("用户名已存在")
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	hashedPassword, err := hash.HashPassword(password)
	if err != nil {
		return nil, err
	}

	newUser := &model.User{
		Username: username,
		Password: hashedPassword,
		Role:     "USER",
	}

	if err := s.userRepo.Create(newUser); err != nil {
		return nil, err
	}

	privateTagID := "PRIVATE_" + username
	privateTagName := username + "的私人空间"

	_, err = s.orgTagRepo.FindByID(privateTagID)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		privateTag := &model.OrganizationTag{
			TagID:       privateTagID,
			Name:        privateTagName,
			Description: "用户的私人组织标签，仅用户本人可访问",
			CreatedBy:   newUser.ID,
		}

		if err := s.orgTagRepo.Create(privateTag); err != nil {
			log.Errorf("[UserService] 创建私人组织标签失败, username: %s, error: %v", username, err)
			return nil, fmt.Errorf("创建私人组织标签失败: %w", err)
		}
	} else if err != nil {
		log.Errorf("[UserService] 查询私人组织标签失败, username: %s, error: %v", username, err)
		return nil, fmt.Errorf("查询私人组织标签失败: %w", err)
	}

	newUser.OrgTags = privateTagID
	newUser.PrimaryOrg = privateTagID

	if err := s.userRepo.Update(newUser); err != nil {
		log.Errorf("[UserService] 更新用户组织标签失败, username: %s, error: %v", username, err)
		return nil, fmt.Errorf("更新用户组织标签失败: %w", err)
	}

	return newUser, nil
}

func (s *userService) Login(username, password string) (accessToken, refreshToken string, err error) {
	user, err := s.userRepo.FindByUsername(username)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", "", errors.New("invalid credentials")
		}
		return "", "", err
	}

	if !hash.CheckPasswordHash(password, user.Password) {
		return "", "", errors.New("invalid credentials")
	}

	accessToken, err = s.jwtManager.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}

	refreshToken, err = s.jwtManager.GenerateRefreshToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}

	return accessToken, refreshToken, nil
}

func (s *userService) GetProfile(username string) (*model.User, error) {
	return s.userRepo.FindByUsername(username)
}

func (s *userService) Logout(tokenString string) error {
	claims, err := s.jwtManager.VerifyToken(tokenString)
	if err != nil {
		return err
	}

	expiration := time.Until(claims.ExpiresAt.Time)

	return database.RDB.Set(
		context.Background(),
		"blacklist:"+tokenString,
		"true",
		expiration,
	).Err()
}

func (s *userService) SetUserPrimaryOrg(username, orgTag string) error {
	user, err := s.userRepo.FindByUsername(username)
	if err != nil {
		return err
	}

	if !strings.Contains(user.OrgTags, orgTag) {
		return errors.New("user does not belong to this organization")
	}

	user.PrimaryOrg = orgTag
	return s.userRepo.Update(user)
}

func (s *userService) GetUserOrgTags(username string) (map[string]interface{}, error) {
	user, err := s.userRepo.FindByUsername(username)
	if err != nil {
		return nil, err
	}

	var orgTags []string
	if user.OrgTags != "" {
		orgTags = strings.Split(user.OrgTags, ",")
	} else {
		orgTags = make([]string, 0)
	}

	orgTagDetails := make([]map[string]string, 0)

	for _, tagID := range orgTags {
		tag, err := s.orgTagRepo.FindByID(tagID)
		if err != nil {
			continue
		}

		orgTagDetails = append(orgTagDetails, map[string]string{
			"tagId":       tag.TagID,
			"name":        tag.Name,
			"description": tag.Description,
		})
	}

	return map[string]interface{}{
		"orgTags":       orgTags,
		"primaryOrg":    user.PrimaryOrg,
		"orgTagDetails": orgTagDetails,
	}, nil
}

func (s *userService) GetUserEffectiveOrgTags(user *model.User) ([]string, error) {
	// 如果用户没有任何组织标签，就没有需要扩展的父级标签，直接返回空切片。
	// OrgTags 是逗号分隔字符串，例如："PRIVATE_tom,ORG_A"。
	if user.OrgTags == "" {
		return []string{}, nil
	}

	// 一次性查询所有组织标签，后面在内存里查父级关系。
	// 这样可以避免每向上找一级父标签就查一次数据库。
	allTags, err := s.orgTagRepo.FindAll()
	if err != nil {
		log.Errorf("[UserService] 获取所有组织标签失败: %v", err)
		return nil, fmt.Errorf("无法获取组织标签列表: %w", err)
	}

	// 构建 tagID -> parentTagID 的映射表。
	// ParentTag 是指针，因为顶级组织没有父级，可以为 nil。
	parentMap := make(map[string]*string)
	for _, tag := range allTags {
		parentMap[tag.TagID] = tag.ParentTag
	}

	// 用 map 模拟 set，用来保存最终有效标签并自动去重。
	// struct{} 不占额外空间，适合只关心 key 是否存在的场景。
	effectiveTags := make(map[string]struct{})
	// 把用户直接拥有的组织标签拆成切片。
	initialTags := strings.Split(user.OrgTags, ",")
	// queue 是队列，用来从用户直接标签开始，逐层向上查找父标签。
	queue := make([]string, 0, len(initialTags))

	// 先把用户直接拥有的标签加入结果集和队列。
	for _, tagID := range initialTags {
		if _, exists := effectiveTags[tagID]; !exists {
			effectiveTags[tagID] = struct{}{}
			// 入队后，后面会继续查这个标签的父级。
			queue = append(queue, tagID)
		}
	}

	// 沿着组织层级向上查找。
	// 例如：TEAM_A -> DEPT_A -> COMPANY。
	for len(queue) > 0 {
		// 取出队首标签。
		currentTagID := queue[0]
		queue = queue[1:]

		// 根据当前标签 ID 查找它的父级标签。
		parentTagPtr, ok := parentMap[currentTagID]
		// ok 表示当前标签存在于 parentMap。
		// parentTagPtr != nil 表示当前标签确实有父级。
		if ok && parentTagPtr != nil {
			// 解引用，拿到父级标签 ID。
			parentTagID := *parentTagPtr

			// 父级标签还没处理过才加入，避免重复处理。
			if _, exists := effectiveTags[parentTagID]; !exists {
				effectiveTags[parentTagID] = struct{}{}
				// 父级标签继续入队，后面还要查父级的父级。
				queue = append(queue, parentTagID)
			}
		}
	}

	// 把 set 转成切片，作为方法返回值。
	result := make([]string, 0, len(effectiveTags))
	for tagID := range effectiveTags {
		result = append(result, tagID)
	}

	// 返回用户直接拥有的标签，以及这些标签向上追溯到的所有父级标签。
	return result, nil
}

func (s *userService) RefreshToken(refreshTokenString string) (newAccessToken, newRefreshToken string, err error) {
	claims, err := s.jwtManager.VerifyToken(refreshTokenString)
	if err != nil {
		return "", "", errors.New("invalid refresh token")
	}

	user, err := s.userRepo.FindByUsername(claims.Username)
	if err != nil {
		return "", "", errors.New("user not found")
	}

	newAccessToken, err = s.jwtManager.GenerateToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}

	newRefreshToken, err = s.jwtManager.GenerateRefreshToken(user.ID, user.Username, user.Role)
	if err != nil {
		return "", "", err
	}

	return newAccessToken, newRefreshToken, nil
}
