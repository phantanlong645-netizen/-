package tasks

// FileProcessingTask 是发送到 Kafka 的文件处理任务。
// 文件上传合并完成后，会把这个任务发到 Kafka。
// 后台消费者拿到任务后，再执行文本解析、分块、向量化、写入 ES。
type FileProcessingTask struct {
	FileMD5   string `json:"file_md5"`
	ObjectUrl string `json:"object_url"`
	FileName  string `json:"file_name"`
	UserID    uint   `json:"user_id"`
	OrgTag    string `json:"org_tag"`
	IsPublic  bool   `json:"is_public"`
}
