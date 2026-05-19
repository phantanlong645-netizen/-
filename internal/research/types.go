package research

import "context"

// Paper 学术论文的核心数据结构
// 统一了不同学术数据库（Semantic Scholar、Arxiv、Crossref等）返回的论文格式
type Paper struct {
	Provider      string   // 数据来源 provider 名称，如 "semantic_scholar"、"arxiv"、"crossref" 等
	ExternalID    string   // 在来源数据库中的唯一标识符，如 DOI、PaperID 或 PubMed ID
	Title         string   // 论文标题
	Abstract      string   // 论文摘要内容
	Authors       []string // 作者列表，每个元素为作者全名
	Year          int      // 发表年份
	URL           string   // 论文的网页 URL（通常是数据库中的论文页面）
	PDFURL        string   // PDF 全文下载链接，如果无法获取则为空字符串
	CitationCount int      // 引用次数，反映论文的学术影响力
}

// SearchTool 学术数据库搜索工具的接口定义
// 所有支持的学术数据库都应实现此接口，以保证检索逻辑的一致性和可扩展性
//
// 设计原则：
// 1. 开闭原则：新增数据源只需实现此接口，无需修改核心检索逻辑
// 2. 依赖倒置：上层模块依赖此抽象接口，而非具体实现
// 3. 单一职责：每个实现只负责与一个特定数据库的交互
type SearchTool interface {
	// Name 返回该搜索工具的名称标识
	// 用于日志记录和结果去重，名称应全局唯一
	Name() string

	// Search 执行学术论文搜索
	// ctx: 上下文对象，用于控制请求超时和取消
	// query: 搜索关键词，可以是论文标题、作者、主题等
	// limit: 返回结果的数量上限
	// 返回: 论文列表和错误信息
	Search(ctx context.Context, query string, limit int) ([]Paper, error)
}
