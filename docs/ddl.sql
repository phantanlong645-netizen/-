CREATE TABLE users (
                       id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '用户唯一标识',
                       username VARCHAR(255) NOT NULL UNIQUE COMMENT '用户名，唯一',
                       password VARCHAR(255) NOT NULL COMMENT '加密后的密码',
                       role ENUM('USER', 'ADMIN') NOT NULL DEFAULT 'USER' COMMENT '用户角色',
                       org_tags VARCHAR(255) DEFAULT NULL COMMENT '用户所属组织标签，多个用逗号分隔',
                       primary_org VARCHAR(50) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL COMMENT '用户主组织标签',
                       created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                       updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                       INDEX idx_username (username) COMMENT '用户名索引'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='用户表';


CREATE TABLE organization_tags (
                                   tag_id VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin PRIMARY KEY COMMENT '标签唯一标识',
                                   name VARCHAR(100) NOT NULL COMMENT '标签名称',
                                   description TEXT COMMENT '描述',
                                   parent_tag VARCHAR(255) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin DEFAULT NULL COMMENT '父标签ID',
                                   created_by BIGINT NOT NULL COMMENT '创建者ID',
                                   created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                                   updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP COMMENT '更新时间',
                                   FOREIGN KEY (parent_tag) REFERENCES organization_tags(tag_id) ON DELETE SET NULL,
                                   FOREIGN KEY (created_by) REFERENCES users(id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='组织标签表';


CREATE TABLE file_upload (
                             id           BIGINT           NOT NULL AUTO_INCREMENT COMMENT '主键',
                             file_md5     VARCHAR(32)      NOT NULL COMMENT '文件 MD5',
                             file_name    VARCHAR(255)     NOT NULL COMMENT '文件名称',
                             total_size   BIGINT           NOT NULL COMMENT '文件大小',
                             status       TINYINT          NOT NULL DEFAULT 0 COMMENT '上传状态',
                             vectorization_status VARCHAR(32) NOT NULL DEFAULT 'PENDING' COMMENT 'vectorization status',
                             vectorization_error_message VARCHAR(1000) DEFAULT '' COMMENT 'vectorization error message',
                             user_id      VARCHAR(64)      NOT NULL COMMENT '用户 ID',
                             org_tag      VARCHAR(50)      DEFAULT NULL COMMENT '组织标签',
                             is_public    TINYINT(1)       NOT NULL DEFAULT 0 COMMENT '是否公开',
                             created_at   TIMESTAMP        NOT NULL DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                             merged_at    TIMESTAMP        NULL DEFAULT NULL ON UPDATE CURRENT_TIMESTAMP COMMENT '合并时间',
                             PRIMARY KEY (id),
                             UNIQUE KEY uk_md5_user (file_md5, user_id),
                             INDEX idx_user (user_id),
                             INDEX idx_org_tag (org_tag)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='文件上传记录';


CREATE TABLE chunk_info (
                            id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '分块记录唯一标识',
                            file_md5 VARCHAR(32) NOT NULL COMMENT '关联的文件MD5值',
                            chunk_index INT NOT NULL COMMENT '分块序号',
                            chunk_md5 VARCHAR(32) NOT NULL COMMENT '分块的MD5值',
                            storage_path VARCHAR(255) NOT NULL COMMENT '分块在存储系统中的路径'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='文件分块信息表';


ALTER TABLE chunk_info
    ADD UNIQUE KEY uk_file_md5_chunk_index (file_md5, chunk_index);

CREATE TABLE document_vectors (
                                  vector_id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '向量记录唯一标识',
                                  file_md5 VARCHAR(32) NOT NULL COMMENT '关联的文件MD5值',
                                  chunk_id INT NOT NULL COMMENT '文本分块序号',
                                  text_content TEXT COMMENT '文本内容',
                                  model_version VARCHAR(32) COMMENT '向量模型版本',
                                  user_id VARCHAR(64) NOT NULL COMMENT '上传用户ID',
                                  org_tag VARCHAR(50) COMMENT '文件所属组织标签',
                                  is_public TINYINT(1) NOT NULL DEFAULT 0 COMMENT '文件是否公开'
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='文档向量存储表';

CREATE TABLE research_sessions (
                                   id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT 'Agent 检索会话 ID',
                                   user_id BIGINT NOT NULL COMMENT '用户 ID',
                                   query VARCHAR(1000) NOT NULL COMMENT '用户原始检索问题',
                                   planned_queries TEXT COMMENT 'Agent 规划出来的检索 query 列表 JSON',
                                   status VARCHAR(32) NOT NULL COMMENT '会话状态',
                                   error_message VARCHAR(1000) DEFAULT '' COMMENT '失败原因',
                                   created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                                   completed_at TIMESTAMP NULL DEFAULT NULL COMMENT '完成时间',
                                   INDEX idx_research_sessions_user (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Agent 外部检索会话表';

CREATE TABLE research_candidates (
                                     id BIGINT AUTO_INCREMENT PRIMARY KEY COMMENT '候选结果 ID',
                                     session_id BIGINT NOT NULL COMMENT '检索会话 ID',
                                     user_id BIGINT NOT NULL COMMENT '用户 ID',
                                     provider VARCHAR(50) NOT NULL COMMENT '来源，例如 semantic_scholar 或 arxiv',
                                     external_id VARCHAR(255) NOT NULL COMMENT '外部系统 ID',
                                     title VARCHAR(1000) NOT NULL COMMENT '标题',
                                     abstract TEXT COMMENT '摘要',
                                     authors TEXT COMMENT '作者 JSON',
                                     year INT DEFAULT 0 COMMENT '年份',
                                     url VARCHAR(1000) DEFAULT '' COMMENT '论文页面地址',
                                     pdf_url VARCHAR(1000) DEFAULT '' COMMENT 'PDF 地址',
                                     citation_count INT DEFAULT 0 COMMENT '引用数',
                                     relevance_score DOUBLE DEFAULT 0 COMMENT 'Agent 相关性分数',
                                     selection_reason VARCHAR(1000) DEFAULT '' COMMENT '入选原因',
                                     import_status VARCHAR(32) NOT NULL DEFAULT 'PENDING' COMMENT '导入状态',
                                     file_md5 VARCHAR(32) DEFAULT '' COMMENT '导入后文件 MD5',
                                     import_error VARCHAR(1000) DEFAULT '' COMMENT '导入失败原因',
                                     created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP COMMENT '创建时间',
                                     imported_at TIMESTAMP NULL DEFAULT NULL COMMENT '导入时间',
                                     INDEX idx_research_candidates_session (session_id),
                                     INDEX idx_research_candidates_user (user_id),
                                     INDEX idx_research_candidate_source (provider, external_id),
                                     INDEX idx_research_candidates_file_md5 (file_md5)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COMMENT='Agent 外部检索候选结果表';


INSERT INTO users (username, password, role) VALUES ('admin', '$2a$10$CuNbcCAjuZPTu/VnBT/kgeU4Pu.bcEo23GJxvugZt/3yTQ8iIF4hC', 'ADMIN');
INSERT INTO users (username, password, role) VALUES ('testuser', '$2a$10$zUiAOXogIuHnNyR7vf8Q3usknDJcvmbc.36Kl2iC0gdAWyrecoGZa', 'USER');

-- 初始化用户对应的私人组织标签，并绑定到用户
INSERT INTO organization_tags (tag_id, name, description, parent_tag, created_by)
SELECT 'PRIVATE_admin', 'admin的私人空间', '用户的私人组织标签，仅用户本人可访问', NULL, id
FROM users WHERE username = 'admin';

INSERT INTO organization_tags (tag_id, name, description, parent_tag, created_by)
SELECT 'PRIVATE_testuser', 'testuser的私人空间', '用户的私人组织标签，仅用户本人可访问', NULL, id
FROM users WHERE username = 'testuser';

UPDATE users SET org_tags = 'PRIVATE_admin', primary_org = 'PRIVATE_admin' WHERE username = 'admin';
UPDATE users SET org_tags = 'PRIVATE_testuser', primary_org = 'PRIVATE_testuser' WHERE username = 'testuser';
