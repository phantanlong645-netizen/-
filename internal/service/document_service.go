package service

import (
	"context"
	"errors"
	"net/url"
	"time"

	"RAG-repository/internal/config"
	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
	"RAG-repository/internal/storagepath"
	"RAG-repository/pkg/es"
	"RAG-repository/pkg/kafka"
	"RAG-repository/pkg/storage"
	"RAG-repository/pkg/tasks"
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
	RetryVectorization(fileMD5 string, user *model.User) (*model.FileUpload, error)
	GenerateDownloadURL(fileMD5 string, fileName string, user *model.User) (*DownloadInfoDTO, error)
	GetFilePreviewContent(fileMD5 string, fileName string, user *model.User) (*PreviewInfoDTO, error)
}

type documentService struct {
	uploadRepo    repository.UploadRepository
	userRepo      repository.UserRepository
	userService   UserService
	orgTagRepo    repository.OrgTagRepository
	docVectorRepo repository.DocumentVectorRepository
	minioCfg      config.MinIOConfig
	esCfg         config.ElasticsearchConfig
	tikaClient    *tika.Client
}

func NewDocumentService(uploadRepo repository.UploadRepository, userRepo repository.UserRepository, userService UserService, orgTagRepo repository.OrgTagRepository, docVectorRepo repository.DocumentVectorRepository, minioCfg config.MinIOConfig, esCfg config.ElasticsearchConfig, tikaClient *tika.Client) DocumentService {
	return &documentService{
		uploadRepo:    uploadRepo,
		userRepo:      userRepo,
		userService:   userService,
		orgTagRepo:    orgTagRepo,
		docVectorRepo: docVectorRepo,
		minioCfg:      minioCfg,
		esCfg:         esCfg,
		tikaClient:    tikaClient,
	}
}

func (s *documentService) ListAccessibleFiles(user *model.User) ([]model.FileUpload, error) {
	orgTags, err := s.userService.GetUserEffectiveOrgTags(user)
	if err != nil {
		return nil, err
	}
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

	if err := es.DeleteByFileMD5(context.Background(), s.esCfg.IndexName, fileMD5); err != nil {
		return err
	}

	if err := s.docVectorRepo.DeleteByFileMD5(fileMD5); err != nil {
		return err
	}

	objectName := storagepath.MergedObjectName(record.UserID, record.FileMD5, record.FileName)
	err = storage.MinioClient.RemoveObject(context.Background(), s.minioCfg.BucketName, objectName, minio.RemoveObjectOptions{})
	if err != nil {
		// 原项目这里忽略 MinIO 删除失败，继续删除数据库记录。
	}

	return s.uploadRepo.DeleteFileUploadRecord(fileMD5, record.UserID)
}

func (s *documentService) RetryVectorization(fileMD5 string, user *model.User) (*model.FileUpload, error) {
	record, err := s.uploadRepo.GetFileUploadRecord(fileMD5, user.ID)
	if err != nil {
		return nil, errors.New("文件不存在或不属于该用户")
	}

	if record.UserID != user.ID && user.Role != "ADMIN" {
		return nil, errors.New("没有权限重试此文件")
	}

	if record.Status != model.FileUploadStatusCompleted {
		return nil, errors.New("文件尚未上传完成，不能重试入库")
	}

	if err := kafka.ResetFileTaskAttempts(fileMD5); err != nil {
		return nil, err
	}

	if err := s.uploadRepo.UpdateFileVectorizationStatus(record.ID, model.VectorizationStatusPending, ""); err != nil {
		return nil, err
	}

	task := tasks.FileProcessingTask{
		FileMD5:  record.FileMD5,
		FileName: record.FileName,
		UserID:   record.UserID,
		OrgTag:   record.OrgTag,
		IsPublic: record.IsPublic,
	}

	if err := kafka.ProduceFileTask(task); err != nil {
		_ = s.uploadRepo.UpdateFileVectorizationStatus(record.ID, model.VectorizationStatusFailed, "send file processing task to Kafka failed: "+err.Error())
		return nil, err
	}

	record.VectorizationStatus = model.VectorizationStatusPending
	record.VectorizationErrorMessage = ""
	return record, nil
}

func (s *documentService) findAccessibleFile(fileMD5 string, fileName string, user *model.User) (*model.FileUpload, error) {
	files, err := s.ListAccessibleFiles(user)
	if err != nil {
		return nil, err
	}

	for i := range files {
		if fileMD5 != "" && files[i].FileMD5 == fileMD5 {
			return &files[i], nil
		}
	}

	if fileMD5 == "" {
		for i := range files {
			if files[i].FileName == fileName {
				return &files[i], nil
			}
		}
	}

	return nil, errors.New("文件不存在或无权访问")
}

func (s *documentService) GenerateDownloadURL(fileMD5 string, fileName string, user *model.User) (*DownloadInfoDTO, error) {
	targetFile, err := s.findAccessibleFile(fileMD5, fileName, user)
	if err != nil {
		return nil, err
	}

	expiry := time.Hour
	objectName := storagepath.MergedObjectName(targetFile.UserID, targetFile.FileMD5, targetFile.FileName)
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

func (s *documentService) GetFilePreviewContent(fileMD5 string, fileName string, user *model.User) (*PreviewInfoDTO, error) {
	targetFile, err := s.findAccessibleFile(fileMD5, fileName, user)
	if err != nil {
		return nil, err
	}

	objectName := storagepath.MergedObjectName(targetFile.UserID, targetFile.FileMD5, targetFile.FileName)
	object, err := storage.MinioClient.GetObject(context.Background(), s.minioCfg.BucketName, objectName, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	defer object.Close()

	content, err := s.tikaClient.ExtractText(object, targetFile.FileName)
	if err != nil {
		return nil, err
	}

	return &PreviewInfoDTO{
		FileName: targetFile.FileName,
		Content:  content,
		FileSize: targetFile.TotalSize,
	}, nil
}
