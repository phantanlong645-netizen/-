# syntax=docker/dockerfile:1

FROM node:20-alpine AS builder

WORKDIR /app

RUN corepack enable

COPY package.json pnpm-lock.yaml pnpm-workspace.yaml .npmrc ./
COPY .env .env.prod ./
COPY build ./build
COPY packages ./packages
COPY public ./public
COPY src ./src
COPY index.html vite.config.ts tsconfig.json uno.config.ts eslint.config.js ./

RUN pnpm install --frozen-lockfile
RUN pnpm build

FROM nginx:1.27-alpine

COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 80

CMD ["nginx", "-g", "daemon off;"]
