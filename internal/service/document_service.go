package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"RAG-repository/internal/config"
	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/pkg/storage"
	"RAG-repository/pkg/tika"

	"github.com/minio/minio-go/v7"
)

type FileUploadDTO struct {
	model.FileUpload
	OrgTagName string `json:"orgTagName"`
}

type DownloadInfoDTO struct {
	FileName    string `json:"fileName"`
	DownloadURL string `json:"downloadUrl"`
	FileSize    int64  `json:"fileSize"`
}

type PreviewInfoDTO struct {
	FileName string `json:"fileName"`
	Content  string `json:"content"`
	FileSize int64  `json:"fileSize"`
}

type DocumentService interface {
	ListAccessibleFiles(user *model.User) ([]model.FileUpload, error)
	ListUploadedFiles(userID uint) ([]FileUploadDTO, error)
	DeleteDocument(fileMD5 string, user *model.User) error
	GenerateDownloadURL(fileName string, user *model.User) (*DownloadInfoDTO, error)
	GetFilePreviewContent(fileName string, user *model.User) (*PreviewInfoDTO, error)
}

type documentService struct {
	uploadRepo repository.UploadRepository
	userRepo   repository.UserRepository
	orgTagRepo repository.OrgTagRepository
	minioCfg   config.MinIOConfig
	tikaClient *tika.Client
}

func NewDocumentService(uploadRepo repository.UploadRepository, userRepo repository.UserRepository, orgTagRepo repository.OrgTagRepository, minioCfg config.MinIOConfig, tikaClient *tika.Client) DocumentService {
	return &documentService{
		uploadRepo: uploadRepo,
		userRepo:   userRepo,
		orgTagRepo: orgTagRepo,
		minioCfg:   minioCfg,
		tikaClient: tikaClient,
	}
}

func (s *documentService) ListAccessibleFiles(user *model.User) ([]model.FileUpload, error) {
	orgTags := strings.Split(user.OrgTags, ",")
	return s.uploadRepo.FindAccessibleFiles(user.ID, orgTags)
}
func (s *documentService) ListUploadedFiles(userID uint) ([]FileUploadDTO, error) {
	files, err := s.uploadRepo.FindFilesByUserID(userID)
	if err != nil {
		return nil, err
	}

	dtos, err := s.mapFileUploadsToDTOs(files)
	if err != nil {
		return nil, err
	}

	return dtos, nil
}

func (s *documentService) mapFileUploadsToDTOs(files []model.FileUpload) ([]FileUploadDTO, error) {
	if len(files) == 0 {
		return []FileUploadDTO{}, nil
	}

	tagIDs := make(map[string]struct{})
	for _, file := range files {
		if file.OrgTag != "" {
			tagIDs[file.OrgTag] = struct{}{}
		}
	}

	tagIDList := make([]string, 0, len(tagIDs))
	for id := range tagIDs {
		tagIDList = append(tagIDList, id)
	}

	tags, err := s.orgTagRepo.FindBatchByIDs(tagIDList)
	if err != nil {
		return nil, err
	}

	tagMap := make(map[string]string)
	for _, tag := range tags {
		tagMap[tag.TagID] = tag.Name
	}

	dtos := make([]FileUploadDTO, len(files))
	for i, file := range files {
		dtos[i] = FileUploadDTO{
			FileUpload: file,
			OrgTagName: tagMap[file.OrgTag],
		}
	}

	return dtos, nil
}
func (s *documentService) DeleteDocument(fileMD5 string, user *model.User) error {
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, user.ID)
	if err != nil {
		return errors.New("文件不存在或不属于该用户")
	}

	if record.UserID != user.ID && user.Role != "ADMIN" {
		return errors.New("没有权限删除此文件")
	}

	objectName := fmt.Sprintf("merged/%s", record.FileName)
	err = storage.MinioClient.RemoveObject(context.Background(), s.minioCfg.BucketName, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		// 原项目这里忽略 MinIO 删除失败，继续删除数据库记录。
	}

	return s.uploadRepo.DeleteFileUploadRecord(fileMD5, record.UserID)
}

func (s *documentService) GenerateDownloadURL(fileName string, user *model.User) (*DownloadInfoDTO, error) {
	files, err := s.ListAccessibleFiles(user)
	if err != nil {
		return nil, err
	}

	var targetFile *model.FileUpload
	for i := range files {
		if files[i].FileName == fileName {
			targetFile = &files[i]
			break
		}
	}

	if targetFile == nil {
		return nil, errors.New("文件不存在或无权访问")
	}

	expiry := time.Hour
	objectName := fmt.Sprintf("uploads/%d/%s", targetFile.UserID, targetFile.FileName)
	presignedURL, err := storage.MinioClient.PresignedGetObject(context.Background(), s.minioCfg.BucketName, objectName, expiry, url.Values{})
	if err != nil {
		return nil, err
	}

	return &DownloadInfoDTO{
		FileName:    targetFile.FileName,
		DownloadURL: presignedURL.String(),
		FileSize:    targetFile.TotalSize,
	}, nil
}

func (s *documentService) GetFilePreviewContent(fileName string, user *model.User) (*PreviewInfoDTO, error) {
	files, err := s.ListAccessibleFiles(user)
	if err != nil {
		return nil, err
	}

	var targetFile *model.FileUpload
	for i := range files {
		if files[i].FileName == fileName {
			targetFile = &files[i]
			break
		}
	}

	if targetFile == nil {
		return nil, errors.New("文件不存在或无权访问")
	}

	objectName := fmt.Sprintf("uploads/%d/%s", targetFile.UserID, targetFile.FileName)
	object, err := storage.MinioClient.GetObject(context.Background(), s.minioCfg.BucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()

	content, err := s.tikaClient.ExtractText(object, fileName)
	if err != nil {
		return nil, err
	}

	return &PreviewInfoDTO{
		FileName: targetFile.FileName,
		Content:  content,
		FileSize: targetFile.TotalSize,
	}, nil
}
