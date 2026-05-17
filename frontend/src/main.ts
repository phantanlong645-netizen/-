import 'vue-markdown-shiki/style';
import 'katex/dist/katex.min.css';
import markdownPlugin from 'vue-markdown-shiki';
import katexPlugin from '@traptitech/markdown-it-katex';
import './plugins/assets';
import { setupAppVersionNotification, setupDayjs, setupIconifyOffline, setupLoading, setupNProgress } from './plugins';
import { setupStore } from './store';
import { setupRouter } from './router';
import { setupI18n } from './locales';
import App from './App.vue';
async function setupApp() {
  setupLoading();

  setupNProgress();

  setupIconifyOffline();

  setupDayjs();

  const app = createApp(App);

  setupStore(app);

  await setupRouter(app);

  setupI18n(app);

  setupAppVersionNotification();

  app.use(markdownPlugin, {
    config: (md: any) => {
      md.use(katexPlugin);
    }
  });

  app.mount('#app');
}

setupApp();
