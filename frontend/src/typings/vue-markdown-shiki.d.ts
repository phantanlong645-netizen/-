import type { App, Component, DefineComponent } from 'vue';
import type MarkdownIt from 'markdown-it';

interface MarkdownOptions {
  theme?: string;
  defaultHighlightLang?: string;
  config?: (md: MarkdownIt) => void;
  [key: string]: any;
}

declare const markdownPlugin: {
  install(app: App, options?: MarkdownOptions): void;
};

export const VueMarkdownIt: Component;
export const VueMarkdownItProvider: DefineComponent<{ options?: MarkdownOptions; class?: string }>;

export default markdownPlugin;

declare module 'vue-markdown-shiki/style';
