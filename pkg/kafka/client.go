package kafka

import (
	"RAG-repository/internal/config"
	"RAG-repository/pkg/database"
	"RAG-repository/pkg/log"
	"RAG-repository/pkg/tasks"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// TaskProcessor 是 Kafka 消费者依赖的任务处理接口。
// 这样 Kafka 包不用直接依赖 pipeline 包，降低耦合。
type TaskProcessor interface {
	Process(ctx context.Context, task tasks.FileProcessingTask) error
}

// producer 是全局 Kafka 生产者。
var producer *kafka.Writer

// InitProducer 初始化 Kafka 生产者。
// 后面文件上传完成后，会通过它发送文件处理任务。
func InitProducer(cfg config.KafkaConfig) {
	producer = &kafka.Writer{
		Addr:     kafka.TCP(cfg.Brokers),
		Topic:    cfg.Topic,
		Balancer: &kafka.LeastBytes{},
	}

	log.Info("Kafka producer initialized successfully")
}

// ProduceFileTask 发送文件处理任务到 Kafka。
func ProduceFileTask(task tasks.FileProcessingTask) error {
	taskBytes, err := json.Marshal(task)
	if err != nil {
		return err
	}

	return producer.WriteMessages(
		context.Background(),
		kafka.Message{
			Value: taskBytes,
		},
	)
}

// StartConsumer 启动 Kafka 消费者，持续消费文件处理任务。
// 当前阶段先写出来，等后面 pipeline.Processor 完成后再接入 main.go。
func StartConsumer(cfg config.KafkaConfig, processor TaskProcessor) {
	r := kafka.NewReader(kafka.ReaderConfig{
		Brokers:  []string{cfg.Brokers},
		Topic:    cfg.Topic,
		GroupID:  "pai-smart-go-consumer",
		MinBytes: 10e3,
		MaxBytes: 10e6,
	})

	log.Infof("Kafka consumer started, listening topic '%s'", cfg.Topic)

	for {
		m, err := r.FetchMessage(context.Background())
		if err != nil {
			log.Error("failed to read message from Kafka", err)
			break
		}

		log.Infof("received Kafka message: offset %d", m.Offset)

		var task tasks.FileProcessingTask
		if err := json.Unmarshal(m.Value, &task); err != nil {
			log.Errorf("failed to unmarshal Kafka message: %v, value: %s", err, string(m.Value))

			if err := r.CommitMessages(context.Background(), m); err != nil {
				log.Errorf("failed to commit invalid Kafka message: %v", err)
			}

			continue
		}

		log.Infof("start processing file task: md5=%s, fileName=%s", task.FileMD5, task.FileName)

		if err := processor.Process(context.Background(), task); err != nil {
			log.Errorf("file task failed: md5=%s, error=%v", task.FileMD5, err)

			attemptsKey := fmt.Sprintf("kafka:attempts:%s", task.FileMD5)
			attempts, incErr := database.RDB.Incr(context.Background(), attemptsKey).Result()
			if incErr == nil {
				_ = database.RDB.Expire(context.Background(), attemptsKey, 24*time.Hour).Err()
			}

			if incErr != nil {
				continue
			}

			if attempts >= 3 {
				log.Errorf("file task failed too many times, commit offset: md5=%s", task.FileMD5)
				if err := r.CommitMessages(context.Background(), m); err != nil {
					log.Errorf("failed to commit Kafka message offset: %v", err)
				}
			}

			continue
		}

		log.Infof("file task processed successfully: md5=%s", task.FileMD5)

		_ = database.RDB.Del(context.Background(), fmt.Sprintf("kafka:attempts:%s", task.FileMD5)).Err()

		if err := r.CommitMessages(context.Background(), m); err != nil {
			log.Errorf("failed to commit Kafka message offset: %v", err)
		}
	}

	if err := r.Close(); err != nil {
		log.Fatalf("failed to close Kafka consumer: %v", err)
	}
}
