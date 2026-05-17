import type { RouteMeta } from 'vue-router';
import ElegantVueRouter from '@elegant-router/vue/vite';
import type { RouteKey } from '@elegant-router/types';

export function setupElegantRouter() {
  return ElegantVueRouter({
    layouts: {
      base: 'src/layouts/base-layout/index.vue',
      blank: 'src/layouts/blank-layout/index.vue'
    },
    routePathTransformer(routeName, routePath) {
      const key = routeName as RouteKey;

      if (key === 'login') {
        const modules: UnionKey.LoginModule[] = ['pwd-login', 'code-login', 'register', 'reset-pwd', 'bind-wechat'];

        const moduleReg = modules.join('|');

        return `/login/:module(${moduleReg})?`;
      }

      return routePath;
    },
    onRouteMetaGen(routeName) {
      const key = routeName as RouteKey;

      const constantRoutes: RouteKey[] = ['login', '403', '404', '500'];
      const routeMetaMap: Partial<Record<RouteKey, Partial<RouteMeta>>> = {
        chat: {
          icon: 'solar:chat-round-call-line-duotone',
          order: 1
        },
        'chat-history': {
          icon: 'solar:hashtag-chat-broken',
          roles: ['ADMIN'],
          order: 2
        },
        'knowledge-base': {
          icon: 'solar:folder-line-duotone',
          order: 3
        },
        'research-agent': {
          icon: 'solar:global-line-duotone',
          order: 4
        },
        'org-tag': {
          icon: 'solar:tag-line-duotone',
          roles: ['ADMIN'],
          order: 5
        },
        user: {
          icon: 'solar:user-line-duotone',
          roles: ['ADMIN'],
          order: 6
        },
        'personal-center': {
          icon: 'solar:people-nearby-line-duotone',
          order: 7
        }
      };

      const meta: Partial<RouteMeta> = {
        title: key,
        i18nKey: `route.${key}` as App.I18n.I18nKey,
        ...routeMetaMap[key]
      };

      if (constantRoutes.includes(key)) {
        meta.constant = true;
      }

      return meta;
    }
  });
}
