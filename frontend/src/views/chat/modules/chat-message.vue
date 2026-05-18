<script setup lang="ts">
// eslint-disable-next-line @typescript-eslint/no-unused-vars
import { nextTick } from 'vue';
import { VueMarkdownIt } from 'vue-markdown-shiki';
import { formatDate } from '@/utils/common';
defineOptions({ name: 'ChatMessage' });

const props = defineProps<{ msg: Api.Chat.Message }>();

const authStore = useAuthStore();

function handleCopy(content: string) {
  navigator.clipboard.writeText(content);
  window.$message?.success('已复制');
}

const chatStore = useChatStore();

// 存储文件名和对应的事件处理
const sourceFiles = ref<Array<{fileName: string, id: string}>>([]);

const thinkingSteps = computed(() => {
  const thinking = props.msg.thinking;
  if (!thinking) return [];

  if (thinking.stepQueries?.length) {
    return thinking.stepQueries.filter(item => item.step || item.queries?.length);
  }

  return (thinking.steps || []).map((step, index) => ({
    step,
    queries: index === 0 ? thinking.queries || [] : []
  }));
});

// 处理来源文件链接的函数
function processSourceLinks(text: string): string {
  // 匹配 (来源#数字: 文件名) 的正则表达式，同时兼容中文冒号 ：
  const sourcePattern = /\(来源#(\d+)[：:]\s*([^)]+)\)/g;

  // 使用本地数组避免在 computed 内读写同一个 reactive ref（防止循环依赖）
  const newFiles: Array<{fileName: string, id: string}> = [];

  const result = text.replace(sourcePattern, (_match, sourceNum, fileName) => {
    // 为文件名创建可点击的链接
    const linkClass = 'source-file-link';
    const encodedFileName = encodeURIComponent(fileName.trim());
    const fileId = `source-file-${newFiles.length}`;

    // 存储文件信息到本地数组
    newFiles.push({
      fileName: encodedFileName,
      id: fileId
    });

    return `(来源#${sourceNum}: <span class="${linkClass}" data-file-id="${fileId}">${fileName.trim()}</span>)`;
  });

  // 统一替换 sourceFiles，避免在 replace 回调中直接写 reactive ref
  sourceFiles.value = newFiles;
  return result;
}

const content = computed(() => {
  chatStore.scrollToBottom?.();
  const rawContent = props.msg.content ?? '';

  // 只对助手消息处理来源链接
  if (props.msg.role === 'assistant') {
    return processSourceLinks(rawContent);
  }

  return rawContent;
});

// 处理内容点击事件（事件委托）
function handleContentClick(event: MouseEvent) {
  const target = event.target as HTMLElement;

  // 检查点击的是否是文件链接
  if (target.classList.contains('source-file-link')) {
    const fileId = target.getAttribute('data-file-id');
    if (fileId) {
      const file = sourceFiles.value.find(f => f.id === fileId);
      if (file) {
        handleSourceFileClick(file.fileName);
      }
    }
  }
}

// 处理来源文件点击事件
async function handleSourceFileClick(fileName: string) {
  const decodedFileName = decodeURIComponent(fileName);
  console.log('点击了来源文件:', decodedFileName);

  try {
    window.$message?.loading(`正在获取文件下载链接: ${decodedFileName}`, {
      duration: 0,
      closable: false
    });

    // 调用文件下载接口
    const { error, data } = await request<Api.Document.DownloadResponse>({
      url: 'documents/download',
      params: {
        fileName: decodedFileName,
        token: authStore.token
      },
      baseURL: '/proxy-api'
    });

    window.$message?.destroyAll();

    if (error) {
      window.$message?.error(`文件下载失败: ${error.response?.data?.message || '未知错误'}`);
      return;
    }

    if (data?.downloadUrl) {
      // 在新窗口打开下载链接
      window.open(data.downloadUrl, '_blank');
      window.$message?.success(`文件下载链接已打开: ${decodedFileName}`);
    } else {
      window.$message?.error('未能获取到下载链接');
    }
  } catch (err) {
    window.$message?.destroyAll();
    console.error('文件下载失败:', err);
    window.$message?.error(`文件下载失败: ${decodedFileName}`);
  }
}
</script>

<template>
  <div class="mb-8 flex-col gap-2">
    <div v-if="msg.role === 'user'" class="flex items-center gap-4">
      <NAvatar class="bg-success">
        <SvgIcon icon="ph:user-circle" class="text-icon-large color-white" />
      </NAvatar>
      <div class="flex-col gap-1">
        <NText class="text-4 font-bold">{{ authStore.userInfo.username }}</NText>
        <NText class="text-3 color-gray-500">{{ formatDate(msg.timestamp) }}</NText>
      </div>
    </div>
    <div v-else class="flex items-center gap-4">
      <NAvatar class="bg-primary">
        <SystemLogo class="text-6 text-white" />
      </NAvatar>
      <div class="flex-col gap-1">
        <NText class="text-4 font-bold">派聪明</NText>
        <NText class="text-3 color-gray-500">{{ formatDate(msg.timestamp) }}</NText>
      </div>
    </div>
    <NText v-if="msg.status === 'pending'">
      <icon-eos-icons:three-dots-loading class="ml-12 mt-2 text-8" />
    </NText>
    <NText v-else-if="msg.status === 'error'" class="ml-12 mt-2 italic">服务器繁忙，请稍后再试</NText>
    <div v-else-if="msg.role === 'assistant'" class="mt-2 pl-12" @click="handleContentClick">
      <NCollapse v-if="msg.thinking" class="thinking-panel mb-3" arrow-placement="right">
        <NCollapseItem title="思维过程" name="thinking">
          <div class="thinking-intent" v-if="msg.thinking.intent">
            <span class="thinking-label">意图</span>
            <span>{{ msg.thinking.intent }}</span>
          </div>
          <div class="thinking-steps">
            <div v-for="(item, index) in thinkingSteps" :key="index" class="thinking-step">
              <div class="thinking-step-title">
                <span class="thinking-index">{{ index + 1 }}</span>
                <span>{{ item.step || '围绕问题检索相关知识' }}</span>
              </div>
              <div v-if="item.queries?.length" class="thinking-queries">
                <span v-for="query in item.queries" :key="query" class="thinking-query">{{ query }}</span>
              </div>
            </div>
          </div>
        </NCollapseItem>
      </NCollapse>
      <VueMarkdownIt :content="content" />
    </div>
    <NText v-else-if="msg.role === 'user'" class="ml-12 mt-2 text-4">{{ content }}</NText>
    <NDivider class="ml-12 w-[calc(100%-3rem)] mb-0! mt-2!" />
    <div class="ml-12 flex gap-4">
      <NButton quaternary @click="handleCopy(msg.content)">
        <template #icon>
          <icon-mynaui:copy />
        </template>
      </NButton>
    </div>
  </div>
</template>

<style scoped lang="scss">
:deep(.source-file-link) {
  color: #1890ff;
  cursor: pointer;
  text-decoration: underline;
  transition: color 0.2s;

  &:hover {
    color: #40a9ff;
    text-decoration: none;
  }

  &:active {
    color: #096dd9;
  }
}

.thinking-panel {
  border: 1px solid rgba(99, 102, 241, 0.18);
  border-radius: 8px;
  background: rgba(99, 102, 241, 0.06);
  padding: 2px 10px;
}

.thinking-intent {
  display: flex;
  gap: 8px;
  align-items: flex-start;
  margin-bottom: 10px;
  color: #374151;
  line-height: 1.6;
}

.thinking-label {
  flex: 0 0 auto;
  border-radius: 4px;
  background: rgba(99, 102, 241, 0.12);
  color: #4f46e5;
  padding: 0 6px;
  font-size: 12px;
}

.thinking-steps {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.thinking-step-title {
  display: flex;
  align-items: center;
  gap: 8px;
  color: #111827;
  font-weight: 600;
  line-height: 1.5;
}

.thinking-index {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 20px;
  height: 20px;
  border-radius: 999px;
  background: #4f46e5;
  color: #fff;
  font-size: 12px;
}

.thinking-queries {
  display: flex;
  flex-wrap: wrap;
  gap: 6px;
  margin-left: 28px;
  margin-top: 6px;
}

.thinking-query {
  border: 1px solid rgba(79, 70, 229, 0.22);
  border-radius: 4px;
  background: rgba(255, 255, 255, 0.72);
  color: #4338ca;
  padding: 2px 8px;
  font-size: 12px;
  line-height: 1.6;
}
</style>
