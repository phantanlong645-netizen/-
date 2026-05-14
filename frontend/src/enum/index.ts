export enum SetupStoreId {
  App = 'app-store',
  Theme = 'theme-store',
  Auth = 'auth-store',
  Route = 'route-store',
  Tab = 'tab-store',
  KnowledgeBase = 'knowledge-base-store',
  Chat = 'chat-store'
}
export enum UploadStatus {
  Uploading = 0,
  Completed = 1,
  Merging = 2,
  Failed = 3,
  Pending = 10,
  Paused = 11,
  Break = 12
}
