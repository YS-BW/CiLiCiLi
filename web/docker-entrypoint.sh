#!/bin/sh
# 仅替换 ${BACKEND_UPSTREAM}，保留 nginx 自身变量（$host/$uri/$http_range 等）。
set -e
CONF=/etc/nginx/conf.d/default.conf
if [ -f "$CONF" ]; then
  # 用 awk 精确替换占位符（不使用 envsubst，避免误替换 nginx 变量）。
  awk -v up="${BACKEND_UPSTREAM:-http://backend:8080}" \
    '{ gsub(/\$\{BACKEND_UPSTREAM\}/, up); print }' "$CONF" > "$CONF.tmp" && mv "$CONF.tmp" "$CONF"
  echo "[entrypoint] BACKEND_UPSTREAM=${BACKEND_UPSTREAM:-http://backend:8080} applied"
fi
