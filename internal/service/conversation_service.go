package service

import (
	"context"

	"RAG-repository/internal/model"
	"RAG-repository/internal/repository"
)

type ConversationService interface {
	GetConversationHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error)
	AddMessageToConversation(ctx context.Context, userID uint, message model.ChatMessage) error
}

type conversationService struct {
	repo repository.ConversationRepository
}

func NewConversationService(repo repository.ConversationRepository) ConversationService {
	return &conversationService{
		repo: repo,
	}
}

func (s *conversationService) GetConversationHistory(ctx context.Context, userID uint) ([]model.ChatMessage, error) {
	conversationID, err := s.repo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return nil, err
	}

	return s.repo.GetConversationHistory(ctx, conversationID)
}

func (s *conversationService) AddMessageToConversation(ctx context.Context, userID uint, message model.ChatMessage) error {
	conversationID, err := s.repo.GetOrCreateConversationID(ctx, userID)
	if err != nil {
		return err
	}

	history, err := s.repo.GetConversationHistory(ctx, conversationID)
	if err != nil {
		return err
	}

	history = append(history, message)

	return s.repo.UpdateConversationHistory(ctx, conversationID, history)
}
