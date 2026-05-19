package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"RAG-repository/internal/model"

	"github.com/redis/go-redis/v9"
)

type ConversationRepository interface {
	GetOrCreateConversationID(ctx context.Context, userID uint) (string, error)
	GetConversationHistory(ctx context.Context, conversationID string) ([]model.ChatMessage, error)
	UpdateConversationHistory(ctx context.Context, conversationID string, messages []model.ChatMessage) error
	GetConversationSummary(ctx context.Context, conversationID string) (*model.ConversationSummary, error)
	UpdateConversationSummary(ctx context.Context, conversationID string, summary *model.ConversationSummary) error
	ClearConversationSummary(ctx context.Context, conversationID string) error
	GetAllUserConversationMappings(ctx context.Context) (map[uint]string, error)
}

type redisConversationRepository struct {
	redisClient *redis.Client
}

func NewConversationRepository(redisClient *redis.Client) ConversationRepository {
	return &redisConversationRepository{redisClient: redisClient}
}

func (r *redisConversationRepository) GetOrCreateConversationID(ctx context.Context, userID uint) (string, error) {
	userKey := fmt.Sprintf("user:%d:current_conversation", userID)

	convID, err := r.redisClient.Get(ctx, userKey).Result()
	if err == redis.Nil {
		convID = fmt.Sprintf("%d-%d", time.Now().UnixNano(), userID)

		if err := r.redisClient.Set(ctx, userKey, convID, 7*24*time.Hour).Err(); err != nil {
			return "", fmt.Errorf("failed to set conversation id: %w", err)
		}

		return convID, nil
	}

	if err != nil {
		return "", fmt.Errorf("failed to get conversation id: %w", err)
	}

	return convID, nil
}

func (r *redisConversationRepository) GetConversationHistory(ctx context.Context, conversationID string) ([]model.ChatMessage, error) {
	key := fmt.Sprintf("conversation:%s", conversationID)

	jsonData, err := r.redisClient.Get(ctx, key).Result()
	if err == redis.Nil {
		return []model.ChatMessage{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("failed to get conversation history: %w", err)
	}

	var messages []model.ChatMessage
	if err := json.Unmarshal([]byte(jsonData), &messages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation history: %w", err)
	}

	return messages, nil
}

func (r *redisConversationRepository) UpdateConversationHistory(ctx context.Context, conversationID string, messages []model.ChatMessage) error {
	key := fmt.Sprintf("conversation:%s", conversationID)

	if len(messages) > 20 {
		messages = messages[len(messages)-20:]
	}

	jsonData, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("failed to marshal conversation history: %w", err)
	}

	if err := r.redisClient.Set(ctx, key, jsonData, 7*24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to set conversation history: %w", err)
	}

	return nil
}

func (r *redisConversationRepository) GetConversationSummary(ctx context.Context, conversationID string) (*model.ConversationSummary, error) {
	key := fmt.Sprintf("conversation:%s:summary", conversationID)

	jsonData, err := r.redisClient.Get(ctx, key).Result()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("failed to get conversation summary: %w", err)
	}

	var summary model.ConversationSummary
	if err := json.Unmarshal([]byte(jsonData), &summary); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation summary: %w", err)
	}

	return &summary, nil
}

func (r *redisConversationRepository) UpdateConversationSummary(ctx context.Context, conversationID string, summary *model.ConversationSummary) error {
	key := fmt.Sprintf("conversation:%s:summary", conversationID)

	if summary == nil || strings.TrimSpace(summary.Content) == "" {
		return r.redisClient.Del(ctx, key).Err()
	}

	jsonData, err := json.Marshal(summary)
	if err != nil {
		return fmt.Errorf("failed to marshal conversation summary: %w", err)
	}

	if err := r.redisClient.Set(ctx, key, jsonData, 7*24*time.Hour).Err(); err != nil {
		return fmt.Errorf("failed to set conversation summary: %w", err)
	}

	return nil
}

func (r *redisConversationRepository) ClearConversationSummary(ctx context.Context, conversationID string) error {
	key := fmt.Sprintf("conversation:%s:summary", conversationID)
	return r.redisClient.Del(ctx, key).Err()
}

func (r *redisConversationRepository) GetAllUserConversationMappings(ctx context.Context) (map[uint]string, error) {
	keys, err := r.redisClient.Keys(ctx, "user:*:current_conversation").Result()
	if err != nil {
		return nil, fmt.Errorf("failed to scan user conversation keys: %w", err)
	}

	result := make(map[uint]string)

	for _, key := range keys {
		var userID uint

		if _, err := fmt.Sscanf(key, "user:%d:current_conversation", &userID); err != nil {
			continue
		}

		convID, err := r.redisClient.Get(ctx, key).Result()
		if err != nil {
			continue
		}

		result[userID] = convID
	}

	return result, nil
}
