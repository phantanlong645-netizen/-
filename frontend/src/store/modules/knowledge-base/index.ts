// 引入请求 ID 的请求头 key，用于给每个上传请求打唯一标识。
import { REQUEST_ID_KEY } from '~/packages/axios/src';
// 引入 nanoid，用于生成每个分片请求的唯一 requestId。
import { nanoid } from '~/packages/utils/src';

// 单个文件内部最多允许同时上传的分片数量。
const maxConcurrentChunksPerFile = 4;

// 定义知识库上传相关的 Pinia store。
export const useKnowledgeBaseStore = defineStore(SetupStoreId.KnowledgeBase, () => {
  // 保存前端本地维护的上传任务列表。
  const tasks = ref<Api.KnowledgeBase.UploadTask[]>([]);
  // 保存当前正在上传的文件 MD5 集合，用来限制文件级并发。
  const activeUploads = ref<Set<string>>(new Set());

  // 合并本地已有分片列表和后端返回的最新分片列表，并去重排序。
  function mergeUploadedChunks(currentUploadedChunks: number[], latestUploadedChunks: number[]) {
    // 用 Set 去重，再按分片下标从小到大排序，保证进度计算稳定。
    return Array.from(new Set([...currentUploadedChunks, ...latestUploadedChunks])).sort((a, b) => a - b);
  }

  // 上传单个分片；chunkIndex 作为参数传入，避免并发时共享 task.chunkIndex 导致串片。
  async function uploadChunk(task: Api.KnowledgeBase.UploadTask, chunkIndex: number): Promise<boolean> {
    // 根据文件总大小和统一分片大小计算总分片数。
    const totalChunks = Math.ceil(task.totalSize / chunkSize);

    // 计算当前分片在文件中的起始字节位置。
    const chunkStart = chunkIndex * chunkSize;
    // 计算当前分片在文件中的结束字节位置，最后一片不能超过文件总大小。
    const chunkEnd = Math.min(chunkStart + chunkSize, task.totalSize);
    // 从原始文件中截取当前分片 Blob。
    const chunk = task.file.slice(chunkStart, chunkEnd);

    // 为当前分片上传请求生成唯一请求 ID。
    const requestId = nanoid();
    // 确保任务对象上存在 requestIds 数组。
    task.requestIds ??= [];
    // 把当前请求 ID 记录到任务上，方便后续追踪或取消。
    task.requestIds.push(requestId);
    // 调用后端分片上传接口。
    const { error, data } = await request<Api.KnowledgeBase.Progress>({
      // 后端接收单个分片的接口地址。
      url: '/upload/chunk',
      // 分片上传使用 POST 请求。
      method: 'POST',
      // multipart/form-data 的请求体数据。
      data: {
        // 当前分片文件内容。
        file: chunk,
        // 整个文件的 MD5，用来标识属于哪个文件。
        fileMd5: task.fileMd5,
        // 当前分片下标。
        chunkIndex,
        // 整个文件的总大小。
        totalSize: task.totalSize,
        // 原始文件名。
        fileName: task.fileName,
        // 文件所属组织标签。
        orgTag: task.orgTag,
        // 文件是否公开。
        isPublic: task.isPublic ?? false
      },
      // 请求头配置。
      headers: {
        // 告诉请求库用表单格式提交文件分片。
        'Content-Type': 'multipart/form-data',
        // 写入当前请求的唯一 ID。
        [REQUEST_ID_KEY]: requestId
      },
      // 单个分片请求最多等待 10 分钟，避免大文件或慢网络过早超时。
      timeout: 10 * 60 * 1000
    });

    // 请求结束后，从任务的请求 ID 列表中移除当前 ID。
    task.requestIds = task.requestIds.filter(id => id !== requestId);

    // 如果请求失败，告诉调用方本分片上传失败。
    if (error) return false;

    // 找到当前文件对应的最新任务对象。
    const updatedTask = tasks.value.find(t => t.fileMd5 === task.fileMd5);
    // 如果任务已不存在，说明页面状态已经变化；分片上传本身仍算成功。
    if (!updatedTask) return true;

    // 记录最近一次完成上传的分片下标。
    updatedTask.chunkIndex = chunkIndex;
    // 合并后端返回的已上传分片列表，避免并发响应互相覆盖。
    updatedTask.uploadedChunks = mergeUploadedChunks(updatedTask.uploadedChunks, data.uploaded);
    // 用已上传分片数量重新计算前端进度百分比。
    updatedTask.progress = Number.parseFloat(((updatedTask.uploadedChunks.length / totalChunks) * 100).toFixed(2));

    // 返回 true 表示当前分片处理成功。
    return true;
  }

  // 并行上传一组分片下标。
  async function uploadChunksInParallel(task: Api.KnowledgeBase.UploadTask, chunkIndexes: number[]) {
    // 如果没有待上传分片，直接返回。
    if (chunkIndexes.length === 0) return;

    // 记录任意 worker 发生的第一个上传错误。
    let uploadError: Error | null = null;
    // worker 数量不能超过配置的分片并发数，也不能超过待上传分片数量。
    const workerCount = Math.min(maxConcurrentChunksPerFile, chunkIndexes.length);

    // 定义一个 worker：每次从队列里取一个分片上传，成功后继续取下一个。
    const runWorker = async (): Promise<void> => {
      // 如果其他 worker 已经失败，则当前 worker 停止继续取任务。
      if (uploadError) return;

      // 从待上传分片队列中取出一个分片下标。
      const chunkIndex = chunkIndexes.shift();
      // 队列取空时，当前 worker 正常结束。
      if (chunkIndex === undefined) return;

      // 上传当前 worker 取到的分片。
      const success = await uploadChunk(task, chunkIndex);
      // 如果上传失败，记录错误并停止当前 worker。
      if (!success) {
        // 保存失败分片信息，后续统一抛出。
        uploadError = new Error(`Chunk ${chunkIndex} upload failed`);
        // 当前 worker 结束。
        return;
      }

      // 当前分片成功后，递归继续处理下一个待上传分片。
      await runWorker();
    };

    // 启动多个 worker，实现单文件内分片并行上传。
    await Promise.all(Array.from({ length: workerCount }, () => runWorker()));

    // 如果任意 worker 失败，整体上传流程视为失败。
    if (uploadError) throw uploadError;
  }

  // 所有分片上传完成后，请求后端合并文件。
  async function mergeFile(task: Api.KnowledgeBase.UploadTask) {
    // 捕获合并过程中的异常，统一返回 false。
    try {
      // 调用后端分片合并接口。
      const { error } = await request({
        // 后端合并分片的接口地址。
        url: '/upload/merge',
        // 合并操作使用 POST 请求。
        method: 'POST',
        // 后端根据 fileMd5 和 fileName 找分片并生成合并文件。
        data: { fileMd5: task.fileMd5, fileName: task.fileName }
      });
      // 后端返回错误时，合并失败。
      if (error) return false;

      // 找到当前文件任务在任务列表中的位置。
      const index = tasks.value.findIndex(t => t.fileMd5 === task.fileMd5);
      // 将任务状态标记为已完成。
      tasks.value[index].status = UploadStatus.Completed;
      tasks.value[index].vectorizationStatus = 'PENDING';
      tasks.value[index].vectorizationErrorMessage = null;
      // 返回 true 表示合并成功。
      return true;
    } catch {
      // 出现异常时，合并失败。
      return false;
    }
  }

  // 将用户选择的文件加入上传队列。
  async function enqueueUpload(form: Api.KnowledgeBase.Form) {
    // 当前上传弹窗只允许一个文件，所以取文件列表的第一个文件。
    const file = form.fileList![0].file!;
    // 计算文件 MD5，用于前后端识别同一个文件。
    const md5 = await calculateMD5(file);

    // 上传前先问后端当前用户是否已经有这个文件，避免重复上传或丢失断点续传进度。
    const { error: checkError, data: checkResult } = await request<Api.KnowledgeBase.CheckFileResult>({
      // 后端根据 MD5 返回 completed 和已上传分片列表。
      url: '/upload/check',
      // 检查接口使用 POST，请求体只需要文件 MD5。
      method: 'POST',
      // md5 对应后端 CheckFileRequest.MD5。
      data: { md5 }
    });
    // 检查失败时无法确认秒传和续传状态，直接中断本次入队。
    if (checkError) return;

    // 如果后端确认文件已经完整上传过，就不再创建本地上传任务。
    if (checkResult.completed) {
      // 展示文件已存在提示。
      window.$message?.error('文件已存在');
      // 结束本次上传入口。
      return;
    }

    // 检查本地任务队列里是否已经有相同文件。
    const existingTask = tasks.value.find(t => t.fileMd5 === md5);
    // 如果已有相同文件任务，需要根据状态决定如何处理。
    if (existingTask) {
      // 如果已经上传完成，提示文件已存在。
      if (existingTask.status === UploadStatus.Completed) {
        // 展示文件重复提示。
        window.$message?.error('文件已存在');
        // 不再重复创建任务。
        return;
      } else if (existingTask.status === UploadStatus.Pending || existingTask.status === UploadStatus.Uploading) {
        // 如果正在等待上传或上传中，提示用户不要重复提交。
        window.$message?.error('文件正在上传中');
        // 不再重复创建任务。
        return;
      } else if (existingTask.status === UploadStatus.Break) {
        // 如果之前中断，则把任务重新放回待上传状态。
        existingTask.status = UploadStatus.Pending;
        // 重新启动上传调度。
        startUpload();
        // 复用旧任务，不创建新任务。
        return;
      }
    }

    // 根据后端返回的已上传分片初始化续传进度。
    const uploadedChunks = mergeUploadedChunks([], checkResult.uploadedChunks ?? []);
    // 根据文件大小计算总分片数。
    const totalChunks = Math.ceil(file.size / chunkSize);
    // 根据后端已有分片计算初始进度。
    const progress = Number.parseFloat(((uploadedChunks.length / totalChunks) * 100).toFixed(2));

    // 创建新的上传任务对象。
    const newTask: Api.KnowledgeBase.UploadTask = {
      // 原始文件对象。
      file,
      // 当前分片内容；并行上传后不再依赖这个字段，保留给类型兼容。
      chunk: null,
      // 最近处理的分片下标。
      chunkIndex: 0,
      // 文件 MD5。
      fileMd5: md5,
      // 文件名。
      fileName: file.name,
      // 文件总大小。
      totalSize: file.size,
      // 兼容旧字段：是否公开。
      public: form.isPublic,
      // 当前使用字段：是否公开。
      isPublic: form.isPublic,
      // 已上传完成的分片下标列表，来自后端 /upload/check，用于断点续传。
      uploadedChunks,
      // 上传进度百分比，按后端已有分片初始化。
      progress,
      // 初始状态为等待上传。
      status: UploadStatus.Pending,
      // 文件合并后才会进入后台知识库处理；上传阶段先标记为等待处理。
      vectorizationStatus: 'PENDING',
      // 后台处理失败时由后端文件列表返回失败原因。
      vectorizationErrorMessage: null,
      // 用户选择的组织标签。
      orgTag: form.orgTag
    };

    // 保存组织标签名称，方便列表展示。
    newTask.orgTagName = form.orgTagName ?? null;

    // 把新任务加入本地上传任务列表。
    tasks.value.push(newTask);
    // 启动上传调度。
    startUpload();
  }

  // 从等待队列中取任务并开始上传。
  async function startUpload() {
    // 文件级并发限制：最多同时上传 3 个文件。
    if (activeUploads.value.size >= 3) return;

    // 找出所有等待上传且不在活跃上传集合中的任务。
    const pendingTasks = tasks.value.filter(
      // 只选择状态为 Pending 且当前没有被 activeUploads 占用的文件。
      t => t.status === UploadStatus.Pending && !activeUploads.value.has(t.fileMd5)
    );

    // 没有待上传任务时直接结束。
    if (pendingTasks.length === 0) return;

    // 取第一个待上传任务开始处理。
    const task = pendingTasks[0];
    // 将任务状态改为上传中。
    task.status = UploadStatus.Uploading;
    // 将当前文件加入活跃上传集合。
    activeUploads.value.add(task.fileMd5);

    // 计算当前文件总分片数量。
    const totalChunks = Math.ceil(task.totalSize / chunkSize);

    // 捕获上传和合并过程中的异常。
    try {
      // 如果所有分片已经上传完成，直接进入合并阶段。
      if (task.uploadedChunks.length === totalChunks) {
        // 请求后端合并文件。
        const success = await mergeFile(task);
        // 合并失败时抛错，进入中断状态。
        if (!success) throw new Error('File merge failed');
        // 合并成功后结束当前任务流程。
        return;
      }

      // 保存剩余待上传分片下标。
      const pendingChunkIndexes: number[] = [];
      // 收集所有还没有上传成功的分片；后端已支持任意分片先到。
      for (let i = 0; i < totalChunks; i += 1) {
        // 只收集还没有上传成功的分片。
        if (!task.uploadedChunks.includes(i)) {
          // 把待上传分片下标加入队列。
          pendingChunkIndexes.push(i);
        }
      }

      // 并行上传所有待上传分片。
      await uploadChunksInParallel(task, pendingChunkIndexes);

      // 重新获取任务对象，拿到并发上传后的最新状态。
      const updatedTask = tasks.value.find(t => t.fileMd5 === task.fileMd5);
      // 如果任务不存在，说明外部状态变化，直接结束。
      if (!updatedTask) return;

      // 上传完成后再次校验分片数量是否齐全。
      if (updatedTask.uploadedChunks.length !== totalChunks) {
        // 分片不完整时抛错，避免提前合并。
        throw new Error('Chunk upload incomplete');
      }

      // 所有分片齐全后，请求后端合并。
      const success = await mergeFile(updatedTask);
      // 合并失败时抛错，进入中断状态。
      if (!success) throw new Error('File merge failed');
    } catch {
      // 上传或合并失败时提示用户，并把任务留在中断状态供后续续传。
      window.$message?.error('文件上传失败');
      // 找到当前任务下标。
      const index = tasks.value.findIndex(t => t.fileMd5 === task.fileMd5);
      // 将当前任务标记为中断，供用户后续续传。
      tasks.value[index].status = UploadStatus.Break;
    } finally {
      // 无论成功失败，都把当前文件从活跃上传集合移除。
      activeUploads.value.delete(task.fileMd5);
      // 尝试继续调度下一个等待中的文件。
      startUpload();
    }
  }

  // 暴露给组件使用的 store 状态和方法。
  return {
    // 上传任务列表。
    tasks,
    // 当前活跃上传文件集合。
    activeUploads,
    // 入队上传方法。
    enqueueUpload,
    // 手动启动上传调度方法。
    startUpload
  };
});
