package kafka

import (
	"RAG-repository/internal/config"
	"RAG-repository/internal/model"
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

// deadLetterProducer 是全局 Kafka 死信生产者。
var deadLetterProducer *kafka.Writer

const (
	maxFileTaskAttempts int64 = 3
	fileTaskTimeout           = 15 * time.Minute
)

// DeadLetterMessage 保存最终无法处理的 Kafka 消息和失败原因。
type DeadLetterMessage struct {
	OriginalTopic     string `json:"original_topic"`
	OriginalPartition int    `json:"original_partition"`
	OriginalOffset    int64  `json:"original_offset"`
	FileMD5           string `json:"file_md5,omitempty"`
	FileName          string `json:"file_name,omitempty"`
	UserID            uint   `json:"user_id,omitempty"`
	Attempts          int64  `json:"attempts"`
	Error             string `json:"error"`
	Payload           string `json:"payload"`
	FailedAt          string `json:"failed_at"`
}

// InitProducer 初始化 Kafka 生产者。
// 后面文件上传完成后，会通过它发送文件处理任务。
func InitProducer(cfg config.KafkaConfig) {
	producer = &kafka.Writer{
		Addr:     kafka.TCP(cfg.Brokers),
		Topic:    cfg.Topic,
		Balancer: &kafka.LeastBytes{},
	}

	deadLetterProducer = &kafka.Writer{
		Addr:     kafka.TCP(cfg.Brokers),
		Topic:    deadLetterTopic(cfg),
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

func deadLetterTopic(cfg config.KafkaConfig) string {
	if cfg.DLTTopic != "" {
		return cfg.DLTTopic
	}
	return cfg.Topic + "-dlt"
}

func ProduceDeadLetterMessage(original kafka.Message, task *tasks.FileProcessingTask, attempts int64, cause error) error {
	if deadLetterProducer == nil {
		return fmt.Errorf("dead letter producer is not initialized")
	}

	deadLetter := DeadLetterMessage{
		OriginalTopic:     original.Topic,
		OriginalPartition: original.Partition,
		OriginalOffset:    original.Offset,
		Attempts:          attempts,
		Error:             trimErrorMessage(cause.Error(), 1000),
		Payload:           string(original.Value),
		FailedAt:          time.Now().Format(time.RFC3339),
	}

	if task != nil {
		deadLetter.FileMD5 = task.FileMD5
		deadLetter.FileName = task.FileName
		deadLetter.UserID = task.UserID
	}

	deadLetterBytes, err := json.Marshal(deadLetter)
	if err != nil {
		return err
	}

	return deadLetterProducer.WriteMessages(
		context.Background(),
		kafka.Message{
			Key:   original.Key,
			Value: deadLetterBytes,
		},
	)
}

func ResetFileTaskAttempts(fileMD5 string) error {
	return database.RDB.Del(context.Background(), fmt.Sprintf("kafka:attempts:%s", fileMD5)).Err()
}

func trimErrorMessage(message string, maxLength int) string {
	if len(message) <= maxLength {
		return message
	}
	return message[:maxLength]
}

func updateFileTaskStatus(task tasks.FileProcessingTask, status string, errorMessage string) error {
	return database.DB.
		Model(&model.FileUpload{}).
		Where("file_md5 = ? AND user_id = ?", task.FileMD5, task.UserID).
		Updates(map[string]interface{}{
			"vectorization_status":        status,
			"vectorization_error_message": errorMessage,
		}).
		Error
}

// StartConsumer ?? Kafka ???,???????????
func StartConsumer(cfg config.KafkaConfig, processor TaskProcessor) {
	// 创建 Kafka reader，也就是消费者客户端。
	r := kafka.NewReader(kafka.ReaderConfig{
		// Brokers 是 Kafka 地址列表，这里配置里目前是一个字符串。
		Brokers: []string{cfg.Brokers},
		// Topic 指定这个消费者要监听哪个 topic。
		Topic: cfg.Topic,
		// GroupID 表示消费者组，同一个组内的消费者会共同消费 topic 分区。
		GroupID: "pai-smart-go-consumer",
		// MinBytes 表示一次 fetch 至少拉取的数据量，Kafka 会尽量攒到这个大小再返回。
		MinBytes: 10e3,
		// MaxBytes 表示一次 fetch 最多拉取的数据量，避免单次响应过大。
		MaxBytes: 10e6,
	})

	// 打印消费者启动日志。
	log.Infof("Kafka consumer started, listening topic '%s'", cfg.Topic)

	// 持续循环消费 Kafka 消息。
	for {
		// FetchMessage 拉取一条消息；这里不会自动提交 offset。
		m, err := r.FetchMessage(context.Background())
		// 如果拉取消息失败，说明 Kafka 读取链路异常，当前实现直接退出消费循环。
		if err != nil {
			// 记录读取 Kafka 消息失败的错误。
			log.Error("failed to read message from Kafka", err)
			// 跳出 for 循环，后面会关闭 reader。
			break
		}

		// 打印当前收到的 Kafka offset，方便排查消费进度。
		log.Infof("received Kafka message: offset %d", m.Offset)

		// 定义文件处理任务结构体，用来接收 Kafka 消息体。
		var task tasks.FileProcessingTask
		// 把 Kafka 消息的 JSON 内容反序列化成 FileProcessingTask。
		if err := json.Unmarshal(m.Value, &task); err != nil {
			// JSON 解析失败说明这条消息格式坏了，重试也大概率没意义。
			log.Errorf("failed to unmarshal Kafka message: %v, value: %s", err, string(m.Value))
			if dltErr := ProduceDeadLetterMessage(m, nil, 0, err); dltErr != nil {
				log.Errorf("failed to send invalid Kafka message to dead letter topic: %v", dltErr)
				continue
			}

			// 提交这条坏消息的 offset，避免消费者一直卡在同一条坏消息上。
			if err := r.CommitMessages(context.Background(), m); err != nil {
				// 如果提交坏消息 offset 失败，记录日志。
				log.Errorf("failed to commit invalid Kafka message: %v", err)
			}

			// 跳过当前消息，继续消费下一条。
			continue
		}

		// 打印即将处理的文件任务信息。
		log.Infof("start processing file task: md5=%s, fileName=%s", task.FileMD5, task.FileName)

		var processErr error
		processed := false
		for attempt := int64(1); attempt <= maxFileTaskAttempts; attempt++ {
			if attempt > 1 {
				time.Sleep(time.Duration(attempt-1) * 2 * time.Second)
			}

			taskCtx, cancel := context.WithTimeout(context.Background(), fileTaskTimeout)
			processErr = processor.Process(taskCtx, task)
			cancel()
			if processErr == nil {
				processed = true
				break
			}

			log.Errorf("file task failed: md5=%s, attempt=%d/%d, error=%v", task.FileMD5, attempt, maxFileTaskAttempts, processErr)
			if attempt < maxFileTaskAttempts {
				message := fmt.Sprintf("第 %d/%d 次处理失败，等待重试: %s", attempt, maxFileTaskAttempts, trimErrorMessage(processErr.Error(), 900))
				if err := updateFileTaskStatus(task, model.VectorizationStatusPending, message); err != nil {
					log.Errorf("failed to mark file upload as pending retry: md5=%s, userID=%d, error=%v", task.FileMD5, task.UserID, err)
					break
				}
			}
		}

		if !processed {
			if dltErr := ProduceDeadLetterMessage(m, &task, maxFileTaskAttempts, processErr); dltErr != nil {
				log.Errorf("failed to send Kafka message to dead letter topic: md5=%s, error=%v", task.FileMD5, dltErr)
				continue
			}

			log.Errorf("file task failed too many times, sent to dead letter topic and commit offset: md5=%s", task.FileMD5)
			if err := updateFileTaskStatus(task, model.VectorizationStatusFailed, trimErrorMessage(processErr.Error(), 1000)); err != nil {
				log.Errorf("failed to mark file upload as failed: md5=%s, userID=%d, error=%v", task.FileMD5, task.UserID, err)
				continue
			}

			if err := r.CommitMessages(context.Background(), m); err != nil {
				log.Errorf("failed to commit Kafka message offset: %v", err)
			}
			continue
		}

		// 走到这里说明 processor.Process 成功完成。
		log.Infof("file task processed successfully: md5=%s", task.FileMD5)

		// 成功后删除 Redis 里的失败次数计数。
		_ = database.RDB.Del(context.Background(), fmt.Sprintf("kafka:attempts:%s", task.FileMD5)).Err()

		// 提交当前消息 offset，表示这条 Kafka 消息已经消费成功。
		if err := r.CommitMessages(context.Background(), m); err != nil {
			// 提交 offset 失败时记录日志；这可能导致后面重复消费同一条消息。
			log.Errorf("failed to commit Kafka message offset: %v", err)
		}
	}

	// 消费循环退出后，关闭 Kafka reader。
	if err := r.Close(); err != nil {
		// reader 关闭失败时记录 fatal 日志。
		log.Fatalf("failed to close Kafka consumer: %v", err)
	}
}
