<script setup lang="ts">
defineOptions({
  name: 'ResearchAgent'
});

const loading = ref(false);
const candidates = ref<Api.KnowledgeBase.ResearchAgent.Candidate[]>([]);
const importedFile = ref<Api.KnowledgeBase.UploadTask | null>(null);
const importingIds = ref<Set<number>>(new Set());
const importPublic = ref(false);

const searchModel = ref<Api.KnowledgeBase.ResearchAgent.SearchRequest>({
  query: '',
  limit: 10
});

const researchSearchTimeout = 120 * 1000;
const researchImportTimeout = 120 * 1000;

function reset() {
  searchModel.value = {
    query: '',
    limit: 10
  };
  candidates.value = [];
  importedFile.value = null;
  importingIds.value = new Set();
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

async function search() {
  const query = searchModel.value.query.trim();
  if (!query) {
    window.$message?.warning('请输入检索问题');
    return;
  }

  loading.value = true;
  candidates.value = [];
  importedFile.value = null;

  const { error, data } = await request<Api.KnowledgeBase.ResearchAgent.SearchResponse>({
    url: '/research-agent/sessions',
    method: 'POST',
    timeout: researchSearchTimeout,
    data: {
      query,
      limit: searchModel.value.limit
    }
  });

  if (!error) {
    candidates.value = data.candidates;
    if (data.candidates.length === 0) window.$message?.info('没有检索到候选结果');
  }

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
              <NButton @click="reset">
                <template #icon>
                  <icon-ic-round-refresh class="text-icon" />
                </template>
                重置
              </NButton>
              <NButton type="primary" ghost :loading="loading" @click="search">
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

    <NCard :bordered="false" size="small" class="sm:flex-1-hidden card-wrapper">
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
                  :disabled="item.importStatus === 'IMPORTED'"
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
