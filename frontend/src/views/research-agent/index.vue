<script setup lang="ts">
import { getAuthorization } from '@/service/request/shared';

defineOptions({
  name: 'ResearchAgent'
});

const loading = ref(false);
const candidates = ref<Api.KnowledgeBase.ResearchAgent.Candidate[]>([]);
const importedFile = ref<Api.KnowledgeBase.UploadTask | null>(null);
const importingIds = ref<Set<number>>(new Set());
const importPublic = ref(false);
const progressLog = ref<Api.KnowledgeBase.ResearchAgent.ProgressEvent[]>([]);
const progressScrollEl = ref<HTMLElement | null>(null);
const sessions = ref<Api.KnowledgeBase.ResearchAgent.Session[]>([]);
const sessionLoading = ref(false);
const abortCtrl = ref<AbortController | null>(null);

const searchModel = ref<Api.KnowledgeBase.ResearchAgent.SearchRequest>({
  query: '',
  limit: 10
});

const researchImportTimeout = 120 * 1000;

// SSE base URL follows the same proxy logic as the axios request instance
const isHttpProxy = import.meta.env.DEV && import.meta.env.VITE_HTTP_PROXY === 'Y';
const sseBaseURL = isHttpProxy ? '/proxy-default' : (import.meta.env.VITE_SERVICE_BASE_URL ?? '');

const selectedCount = computed(() => progressLog.value.filter(e => e.type === 'select').length);

function reset() {
  searchModel.value = {
    query: '',
    limit: 10
  };
  candidates.value = [];
  progressLog.value = [];
  importedFile.value = null;
  importingIds.value = new Set();
}

function cancelSearch() {
  abortCtrl.value?.abort();
  abortCtrl.value = null;
  loading.value = false;
  window.$message?.info('已取消检索');
}

function setImporting(candidateId: number, importing: boolean) {
  const next = new Set(importingIds.value);
  if (importing) next.add(candidateId);
  else next.delete(candidateId);
  importingIds.value = next;
}

function formatProvider(provider: string) {
  if (provider === 'semantic_scholar') return 'Semantic Scholar';
  if (provider === 'arxiv') return 'arXiv';
  if (provider === 'crossref') return 'Crossref';
  if (provider === 'openalex') return 'OpenAlex';
  if (provider === 'pubmed') return 'PubMed';
  return provider;
}

function parseAuthors(authors: string) {
  try {
    const parsed = JSON.parse(authors) as string[];
    return parsed.filter(Boolean).join(', ');
  } catch {
    return authors;
  }
}

function openURL(url?: string) {
  if (!url) return;
  window.open(url, '_blank', 'noopener,noreferrer');
}

function formatTime(t: string) {
  return dayjs(t).format('MM-DD HH:mm');
}

function eventLabel(e: Api.KnowledgeBase.ResearchAgent.ProgressEvent): string {
  switch (e.type) {
    case 'reflect':
      return e.message ?? 'Critic 审核中…';
    case 'reflect_keep':
      return `✓ 保留：${e.title ?? ''}`;
    case 'reflect_reject':
      return `✕ 拒绝：${e.title ?? ''}（${e.reason ?? ''}）`;
    case 'think':
      return e.message ?? '';
    case 'search':
      return `搜索 ${e.source ?? ''}: ${e.query ?? ''}`;
    case 'found':
      return `在 ${e.source ?? ''} 找到 ${e.count ?? 0} 篇`;
    case 'select':
      return `已选：${e.title ?? ''}`;
    case 'finish':
      return e.message ?? '检索完成';
    case 'done':
      return e.message ?? '完成';
    case 'error':
      return `错误：${e.message ?? ''}`;
    default:
      return e.message ?? '';
  }
}

async function loadSessions() {
  sessionLoading.value = true;
  const { error, data } = await request<Api.KnowledgeBase.ResearchAgent.Session[]>({
    url: '/research-agent/sessions?limit=10',
    method: 'GET'
  });
  sessionLoading.value = false;
  if (!error) sessions.value = data;
}

async function loadSession(sessionId: number, query: string) {
  searchModel.value.query = query;
  candidates.value = [];
  progressLog.value = [];
  const { error, data } = await request<Api.KnowledgeBase.ResearchAgent.Candidate[]>({
    url: `/research-agent/sessions/${sessionId}/candidates`,
    method: 'GET'
  });
  if (!error) {
    candidates.value = data;
    if (data.length === 0) window.$message?.info('该会话暂无候选结果');
  }
}

async function search() {
  const query = searchModel.value.query.trim();
  if (!query) {
    window.$message?.warning('请输入检索问题');
    return;
  }

  loading.value = true;
  candidates.value = [];
  progressLog.value = [];
  importedFile.value = null;

  const ctrl = new AbortController();
  abortCtrl.value = ctrl;

  const auth = getAuthorization();
  const headers: Record<string, string> = { 'Content-Type': 'application/json' };
  if (auth) headers['Authorization'] = auth;

  try {
    const response = await fetch(`${sseBaseURL}/research-agent/sessions/stream`, {
      method: 'POST',
      headers,
      body: JSON.stringify({ query, limit: searchModel.value.limit }),
      signal: ctrl.signal
    });

    if (!response.ok || !response.body) {
      window.$message?.error(`检索请求失败 (${response.status})`);
      loading.value = false;
      abortCtrl.value = null;
      return;
    }

    const reader = response.body.getReader();
    const decoder = new TextDecoder();
    let buffer = '';
    let sessionId: number | null = null;

    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      buffer += decoder.decode(value, { stream: true });
      const lines = buffer.split('\n');
      buffer = lines.pop() ?? '';

      for (const line of lines) {
        if (!line.startsWith('data: ')) continue;
        const raw = line.slice(6).trim();
        if (!raw) continue;
        try {
          const evt = JSON.parse(raw) as Api.KnowledgeBase.ResearchAgent.ProgressEvent;
          if (evt.type === 'result') {
            sessionId = evt.session_id ?? null;
          } else {
            progressLog.value.push(evt);
            nextTick(() => {
              if (progressScrollEl.value) {
                progressScrollEl.value.scrollTop = progressScrollEl.value.scrollHeight;
              }
            });
          }
        } catch {
          // ignore malformed SSE frames
        }
      }
    }

    // Fetch candidates after stream ends
    if (sessionId) {
      const { error, data } = await request<Api.KnowledgeBase.ResearchAgent.Candidate[]>({
        url: `/research-agent/sessions/${sessionId}/candidates`,
        method: 'GET'
      });
      if (!error) {
        candidates.value = data;
        if (data.length === 0) window.$message?.info('没有检索到候选结果');
      }
    }

    loadSessions();
  } catch (e: unknown) {
    if (e instanceof Error && e.name !== 'AbortError') {
      window.$message?.error('检索连接失败，请重试');
    }
  }

  abortCtrl.value = null;
  loading.value = false;
}

async function importCandidate(candidate: Api.KnowledgeBase.ResearchAgent.Candidate) {
  if (candidate.importStatus === 'IMPORTED') {
    window.$message?.info('该候选结果已导入');
    return;
  }

  setImporting(candidate.id, true);
  const { error, data } = await request<Api.KnowledgeBase.UploadTask>({
    url: `/research-agent/candidates/${candidate.id}/import`,
    method: 'POST',
    timeout: researchImportTimeout,
    data: {
      orgTag: '',
      isPublic: importPublic.value
    }
  });
  setImporting(candidate.id, false);

  if (error) return;

  candidate.importStatus = 'IMPORTED';
  candidate.fileMd5 = data.fileMd5;
  importedFile.value = data;
  window.$message?.success('已导入知识库，后台正在处理');
}

onMounted(() => loadSessions());
</script>

<template>
  <div class="min-h-500px flex-col-stretch gap-16px overflow-hidden lt-sm:overflow-auto">
    <NCard title="Agent 检索" :bordered="false" size="small" class="card-wrapper">
      <NForm label-placement="left" :label-width="72" :show-feedback="false">
        <NGrid :cols="24" :x-gap="16" :y-gap="12">
          <NFormItemGi label="结果数量" :span="5">
            <NInputNumber v-model:value="searchModel.limit" :min="1" :max="30" :precision="0" class="w-full" />
          </NFormItemGi>
          <NFormItemGi label="检索问题" :span="13">
            <NInput
              v-model:value="searchModel.query"
              clearable
              placeholder="例如：multi-hop question answering retrieval augmented generation"
              @keyup.enter="search"
            />
          </NFormItemGi>
          <NFormItemGi :span="6">
            <NSpace justify="end" class="w-full">
              <NSpace align="center" :size="6">
                <NSwitch v-model:value="importPublic" size="small" />
                <NText class="text-13px">{{ importPublic ? '公开导入' : '私有导入' }}</NText>
              </NSpace>
              <NButton :loading="loading" @click="reset">
                <template #icon>
                  <icon-ic-round-refresh class="text-icon" />
                </template>
                重置
              </NButton>
              <NButton v-if="loading" type="error" ghost @click="cancelSearch">
                <template #icon>
                  <icon-ic-round-stop class="text-icon" />
                </template>
                取消
              </NButton>
              <NButton v-else type="primary" ghost :loading="loading" @click="search">
                <template #icon>
                  <icon-ic-round-search class="text-icon" />
                </template>
                检索
              </NButton>
            </NSpace>
          </NFormItemGi>

          <NFormItemGi v-if="importedFile" :span="24">
            <NTag type="success" :bordered="false">已导入：{{ importedFile.fileName }}</NTag>
          </NFormItemGi>
        </NGrid>
      </NForm>
    </NCard>

    <!-- Agent 实时进度日志 -->
    <NCard
      v-if="progressLog.length > 0 || loading"
      :bordered="false"
      size="small"
      class="card-wrapper"
    >
      <template #header>
        <NSpace align="center" :size="8">
          <span>Agent 进度</span>
          <NTag v-if="selectedCount > 0" size="small" type="success" :bordered="false">
            已选 {{ selectedCount }} / {{ searchModel.limit }}
          </NTag>
        </NSpace>
      </template>
      <div
        ref="progressScrollEl"
        class="max-h-220px overflow-y-auto space-y-4px pr-4px font-mono text-12px"
      >
        <div
          v-for="(evt, idx) in progressLog"
          :key="idx"
          class="flex items-start gap-8px leading-5"
          :class="{
            'text-gray-400 italic': evt.type === 'think',
            'text-blue-500': evt.type === 'search',
            'text-green-500': evt.type === 'found' || evt.type === 'select' || evt.type === 'reflect_keep',
            'text-orange-500 font-semibold': evt.type === 'finish' || evt.type === 'done' || evt.type === 'reflect',
            'text-red-500': evt.type === 'error' || evt.type === 'reflect_reject'
          }"
        >
          <span class="mt-1px shrink-0 text-10px opacity-60">
            <template v-if="evt.type === 'think'">💭</template>
            <template v-else-if="evt.type === 'search'">🔍</template>
            <template v-else-if="evt.type === 'found'">📄</template>
            <template v-else-if="evt.type === 'select'">✅</template>
            <template v-else-if="evt.type === 'finish' || evt.type === 'done'">🏁</template>
            <template v-else-if="evt.type === 'error'">⚠️</template>
            <template v-else-if="evt.type === 'reflect' || evt.type === 'reflect_keep' || evt.type === 'reflect_reject'">🔬</template>
            <template v-else>·</template>
          </span>
          <span class="break-all">{{ eventLabel(evt) }}</span>
        </div>
        <div v-if="loading" class="flex items-center gap-6px text-gray-400">
          <NSpin :size="12" />
          <span>Agent 运行中…</span>
        </div>
      </div>
    </NCard>

    <NCard :bordered="false" size="small" class="sm:flex-1-hidden card-wrapper">
      <template #header>
        <NSpace align="center" justify="space-between" class="w-full">
          <NSpace align="center" :size="8">
            <span>候选论文</span>
            <NTag v-if="candidates.length > 0" size="small" :bordered="false">{{ candidates.length }}</NTag>
          </NSpace>
          <!-- 历史会话列表 -->
          <NPopover v-if="sessions.length > 0" placement="bottom-end" trigger="click" :width="320">
            <template #trigger>
              <NButton size="small" quaternary>
                <template #icon><icon-ic-round-history class="text-icon" /></template>
                历史 ({{ sessions.length }})
              </NButton>
            </template>
            <NSpin :show="sessionLoading">
              <NList hoverable clickable size="small">
                <NListItem
                  v-for="s in sessions"
                  :key="s.id"
                  @click="loadSession(s.id, s.query)"
                >
                  <div class="flex items-center justify-between gap-8px">
                    <NEllipsis class="flex-1 text-13px">{{ s.query }}</NEllipsis>
                    <NSpace :size="4" :wrap="false">
                      <NTag size="small" :type="s.status === 'COMPLETED' ? 'success' : 'error'" :bordered="false">
                        {{ s.status === 'COMPLETED' ? '完成' : '失败' }}
                      </NTag>
                      <NText class="shrink-0 text-11px text-gray-400">{{ formatTime(s.createdAt) }}</NText>
                    </NSpace>
                  </div>
                </NListItem>
              </NList>
            </NSpin>
          </NPopover>
        </NSpace>
      </template>
      <NSpin :show="loading">
        <NEmpty v-if="candidates.length === 0" description="暂无候选结果" class="py-100px" />
        <NScrollbar v-else class="max-h-[calc(100vh-280px)] pr-8px">
          <NCard
            v-for="item in candidates"
            :key="item.id"
            class="my-8"
            embedded
            :bordered="false"
            :segmented="{ content: true, footer: 'soft' }"
          >
            <div class="flex items-start justify-between gap-16px">
              <div class="min-w-0 flex-1">
                <div class="mb-8px flex flex-wrap items-center gap-8px">
                  <NTag size="small" type="info">{{ formatProvider(item.provider) }}</NTag>
                  <NTag v-if="item.year" size="small" :bordered="false">{{ item.year }}</NTag>
                  <NTag v-if="item.citationCount" size="small" :bordered="false">
                    引用 {{ item.citationCount }}
                  </NTag>
                  <NTag v-if="item.importStatus === 'IMPORTED'" size="small" type="success">已导入</NTag>
                  <NTag v-else-if="item.importStatus === 'FAILED'" size="small" type="error">导入失败</NTag>
                </div>
                <div class="mb-6px text-16px font-medium leading-6">{{ item.title }}</div>
                <NEllipsis v-if="parseAuthors(item.authors)" :line-clamp="1" class="mb-8px text-12px text-gray-500">
                  {{ parseAuthors(item.authors) }}
                </NEllipsis>
                <NEllipsis :line-clamp="4" class="text-13px leading-6">
                  {{ item.abstract || '暂无摘要' }}
                </NEllipsis>
              </div>
              <NSpace vertical align="end" :size="8">
                <NTag :bordered="false">Score {{ item.relevanceScore.toFixed(1) }}</NTag>
                <NButton size="small" ghost :disabled="!item.url" @click="openURL(item.url)">
                  <template #icon>
                    <icon-ic-round-open-in-new class="text-icon" />
                  </template>
                  页面
                </NButton>
                <NButton size="small" ghost :disabled="!item.pdfUrl" @click="openURL(item.pdfUrl)">
                  <template #icon>
                    <icon-ic-round-picture-as-pdf class="text-icon" />
                  </template>
                  PDF
                </NButton>
                <NButton
                  size="small"
                  type="primary"
                  ghost
                  :loading="importingIds.has(item.id)"
                  :disabled="item.importStatus === 'IMPORTED' || !item.pdfUrl"
                  @click="importCandidate(item)"
                >
                  <template #icon>
                    <icon-ic-round-library-add class="text-icon" />
                  </template>
                  导入
                </NButton>
              </NSpace>
            </div>
            <template #footer>
              <NEllipsis :line-clamp="2">入选原因：{{ item.selectionReason || '按标题、摘要和引用量排序' }}</NEllipsis>
            </template>
          </NCard>
        </NScrollbar>
      </NSpin>
    </NCard>
  </div>
</template>

<style scoped></style>
