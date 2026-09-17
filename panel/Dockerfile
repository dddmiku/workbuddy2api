# syntax=docker/dockerfile:1
# ═══ 更新日志 ═══
# 2026-09-17：新增管理面板镜像：标准库 Python + docker CLI，供同一 Compose 项目内运行。
FROM python:3.12-alpine

# docker CLI：面板用 inspect / restart / exec / logs 操作同机网关容器。
RUN apk add --no-cache docker-cli tzdata \
 && adduser -D -u 10002 -H panel

WORKDIR /app
COPY app.py key_management.py index.html login.html ./
COPY vendor/ ./vendor/

# 运行期数据（credentials.json、initial-password.txt）放挂载卷，镜像内不落凭证。
ENV WB2API_ADMIN_HOST=0.0.0.0 \
    WB2API_ADMIN_PORT=7864 \
    WB2API_ADMIN_DIR=/data \
    WB2API_GATEWAY_DIR=/gateway \
    WB2API_GATEWAY_URL=http://wb2api:7863 \
    WB2API_CONTAINER=workbuddy2api \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1

# 面板需要访问 docker.sock（宿主 root 拥有）并改写网关 config.json，
# 因此以 root 运行；它不监听公网，只由宿主机端口映射暴露到回环地址。
EXPOSE 7864
HEALTHCHECK --interval=30s --timeout=5s --start-period=5s \
  CMD python3 -c "import urllib.request,os;urllib.request.urlopen('http://127.0.0.1:%s/login' % os.environ.get('WB2API_ADMIN_PORT','7864'),timeout=4)" || exit 1
ENTRYPOINT ["python3", "/app/app.py"]
