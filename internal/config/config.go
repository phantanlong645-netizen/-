package config

import (
	"fmt"

	"github.com/spf13/viper"
)

var Conf Config

type Config struct {
	Server        ServerConfig        `mapstructure:"server"`
	Database      DatabaseConfig      `mapstructure:"database"`
	JWT           JWTConfig           `mapstructure:"jwt"`
	Log           LogConfig           `mapstructure:"log"`
	Kafka         KafkaConfig         `mapstructure:"kafka"`
	MinIO         MinIOConfig         `mapstructure:"minio"`
	Tika          TikaConfig          `mapstructure:"tika"`
	Elasticsearch ElasticsearchConfig `mapstructure:"elasticsearch"`
	Embedding     EmbeddingConfig     `mapstructure:"embedding"`
	LLM           LLMConfig           `mapstructure:"llm"`
	AI            AIConfig            `mapstructure:"ai"`
	ResearchAgent ResearchAgentConfig `mapstructure:"research_agent"`
	LightRAG      LightRAGConfig      `mapstructure:"lightrag"`
}

type LightRAGConfig struct {
	URL    string `mapstructure:"url"`
	APIKey string `mapstructure:"api_key"`
	Enable bool   `mapstructure:"enable"`
}
type LLMConfig struct {
	APIKey      string              `mapstructure:"api_key"`
	BaseURL     string              `mapstructure:"base_url"`
	Model       string              `mapstructure:"model"`
	IntentModel string              `mapstructure:"intent_model"`
	Generation  LLMGenerationConfig `mapstructure:"generation"`
	Prompt      LLMPromptConfig     `mapstructure:"prompt"`
}

type LLMGenerationConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top_p"`
	MaxTokens   int     `mapstructure:"max_tokens"`
}

type LLMPromptConfig struct {
	Rules        string `mapstructure:"rules"`
	RefStart     string `mapstructure:"ref_start"`
	RefEnd       string `mapstructure:"ref_end"`
	NoResultText string `mapstructure:"no_result_text"`
}

type AIConfig struct {
	Generation AIGenerationConfig `mapstructure:"generation"`
	Prompt     AIPromptConfig     `mapstructure:"prompt"`
}

type AIGenerationConfig struct {
	Temperature float64 `mapstructure:"temperature"`
	TopP        float64 `mapstructure:"top-p"`
	MaxTokens   int     `mapstructure:"max-tokens"`
}

type AIPromptConfig struct {
	Rules        string `mapstructure:"rules"`
	RefStart     string `mapstructure:"ref-start"`
	RefEnd       string `mapstructure:"ref-end"`
	NoResultText string `mapstructure:"no-result-text"`
}

type ServerConfig struct {
	Port string `mapstructure:"port"`
	Mode string `mapstructure:"mode"`
}

type DatabaseConfig struct {
	MySQL MySQLConfig `mapstructure:"mysql"`
	Redis RedisConfig `mapstructure:"redis"`
}

type MySQLConfig struct {
	DSN string `mapstructure:"dsn"`
}

type RedisConfig struct {
	Addr     string `mapstructure:"addr"`
	Password string `mapstructure:"password"`
	DB       int    `mapstructure:"db"`
}

type JWTConfig struct {
	Secret                 string `mapstructure:"secret"`
	AccessTokenExpireHours int    `mapstructure:"access_token_expire_hours"`
	RefreshTokenExpireDays int    `mapstructure:"refresh_token_expire_days"`
}

type LogConfig struct {
	Level      string `mapstructure:"level"`
	Format     string `mapstructure:"format"`
	OutputPath string `mapstructure:"output_path"`
}

type KafkaConfig struct {
	Brokers       string `mapstructure:"brokers"`
	Topic         string `mapstructure:"topic"`
	DLTTopic      string `mapstructure:"dlt_topic"`
	ConsumerCount int    `mapstructure:"consumer_count"`
}

type MinIOConfig struct {
	Endpoint        string `mapstructure:"endpoint"`
	AccessKeyID     string `mapstructure:"access_key_id"`
	SecretAccessKey string `mapstructure:"secret_access_key"`
	UseSSL          bool   `mapstructure:"use_ssl"`
	BucketName      string `mapstructure:"bucket_name"`
}

type TikaConfig struct {
	ServerURL string `mapstructure:"server_url"`
}

type ElasticsearchConfig struct {
	Addresses string `mapstructure:"addresses"`
	Username  string `mapstructure:"username"`
	Password  string `mapstructure:"password"`
	IndexName string `mapstructure:"index_name"`
}

type EmbeddingConfig struct {
	Model      string `mapstructure:"model"`
	APIKey     string `mapstructure:"api_key"`
	BaseURL    string `mapstructure:"base_url"`
	Dimensions int    `mapstructure:"dimensions"`
}

type ResearchAgentConfig struct {
	SemanticScholarAPIKey string `mapstructure:"semantic_scholar_api_key"`
	MaxCandidates         int    `mapstructure:"max_candidates"`
}

func Init(configPath string) {
	viper.SetConfigFile(configPath)
	viper.SetConfigType("yaml")

	if err := viper.ReadInConfig(); err != nil {
		panic(fmt.Errorf("读取配置文件失败: %w", err))
	}

	if err := viper.Unmarshal(&Conf); err != nil {
		panic(fmt.Errorf("无法将配置解析到结构体中: %w", err))
	}
}
