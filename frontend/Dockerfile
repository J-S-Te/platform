# syntax=docker/dockerfile:1

# Vite 在构建时写入 API 基础路径，因此通过构建参数固定为同源反向代理地址。
FROM node:22-alpine AS builder

WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci

COPY . ./

ARG VITE_API_BASE_URL=/api/v1
ARG VITE_LOGIN_SUCCESS_URL=/
ENV VITE_API_BASE_URL=${VITE_API_BASE_URL}
ENV VITE_LOGIN_SUCCESS_URL=${VITE_LOGIN_SUCCESS_URL}

RUN npm run build

# 前端静态资源由 Nginx 提供，并将 API、OIDC 端点代理至后端 API 容器。
FROM nginx:1.27-alpine

COPY nginx/default.conf /etc/nginx/conf.d/default.conf
COPY --from=builder /app/dist /usr/share/nginx/html

EXPOSE 80
