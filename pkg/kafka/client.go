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

		// 调用真正的文件处理逻辑：下载文件、提取文本、分块、向量化、写 ES。
		if err := processor.Process(context.Background(), task); err != nil {
			// 处理失败时，先记录失败原因。
			log.Errorf("file task failed: md5=%s, error=%v", task.FileMD5, err)

			// 用 fileMD5 拼出 Redis 失败次数 key。
			attemptsKey := fmt.Sprintf("kafka:attempts:%s", task.FileMD5)
			// 失败次数加 1；这个计数用于限制最多重试几次。
			attempts, incErr := database.RDB.Incr(context.Background(), attemptsKey).Result()
			// 如果 Redis 计数成功，给这个失败计数设置 24 小时过期时间。
			if incErr == nil {
				// Expire 失败不影响主流程，所以这里忽略返回错误。
				_ = database.RDB.Expire(context.Background(), attemptsKey, 24*time.Hour).Err()
			}

			// 如果 Redis 失败次数写入失败，当前消息不提交 offset，让 Kafka 后续继续重试。
			if incErr != nil {
				// 不 commit，直接进入下一轮循环。
				continue
			}

			// 如果同一个文件任务已经失败 3 次及以上，就放弃继续重试。
			if attempts >= 3 {
				// 打印放弃重试并提交 offset 的日志。
				log.Errorf("file task failed too many times, commit offset: md5=%s", task.FileMD5)
				if err := database.DB.
					Model(&model.FileUpload{}).
					Where("file_md5 = ? AND user_id = ?", task.FileMD5, task.UserID).
					Update("status", model.FileUploadStatusFailed).
					Error; err != nil {
					log.Errorf("failed to mark file upload as failed: md5=%s, userID=%d, error=%v", task.FileMD5, task.UserID, err)
					continue
				}
				// 提交 offset，Kafka 会认为这条消息已经处理完成，不会再投递。
				if err := r.CommitMessages(context.Background(), m); err != nil {
					// 如果提交 offset 失败，记录日志。
					log.Errorf("failed to commit Kafka message offset: %v", err)
				}
			}

			// 处理失败后结束当前消息流程；如果没提交 offset，后面会重试。
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
