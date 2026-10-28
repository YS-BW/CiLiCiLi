# CiLiCiLi Web 镜像（Nginx 托管静态产物 + 同源反代 /api）。
#
# 默认：使用宿主已构建的 `dist/`（`npm run build` 产物），镜像只负责托管。
#       适合构建机无外网 / 已在前端目录构建好的场景。
# 完整自包含构建：使用 Dockerfile.full（多阶段 node→nginx），需要构建机可访问 npm 源。
FROM nginx:1.27-alpine
COPY nginx.conf /etc/nginx/conf.d/default.conf
COPY docker-entrypoint.sh /docker-entrypoint.d/40-substitute-upstream.sh
RUN chmod +x /docker-entrypoint.d/40-substitute-upstream.sh
COPY dist /usr/share/nginx/html
ENV BACKEND_UPSTREAM=http://backend:8080
EXPOSE 80
