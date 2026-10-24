FROM alpine:3
ENV TZ=Asia/Shanghai \
    SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY zoneinfo /usr/share/zoneinfo
COPY cilicili /usr/local/bin/cilicili
COPY web /web
COPY sources.json /etc/cilicili/sources.json
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/cilicili"]
