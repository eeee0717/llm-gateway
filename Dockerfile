# 多阶段构建。最终镜像用 distroless：只有两个静态二进制，没有 shell，也没有包管理器。
FROM golang:1.27 AS build
WORKDIR /src

# 依赖清单单独成一层：只要 go.mod/go.sum 没变，下次构建就不用重新下载。
COPY go.mod go.sum ./
RUN go mod download

COPY . .
# CGO_ENABLED=0 才是静态链接，distroless/static 里没有 libc。
# -s -w 去掉符号表和调试信息，二进制小一半。
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/ ./cmd/gateway ./cmd/mockupstream

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gateway /out/mockupstream /
# 业务端口和管理端口。管理端口不要往公网暴露。
EXPOSE 8080 8081
ENTRYPOINT ["/gateway"]
CMD ["serve", "-config", "/etc/gateway/config.yaml"]
