# Стадия сборки: toolchain Go и кеш модулей в итоговый образ не попадают.
FROM golang:1.27 AS build
# Версия вшивается в бинарник и видна в GET /version: docker build --build-arg VERSION=1.0.1 ...
ARG VERSION=dev
WORKDIR /src
# Сначала только список зависимостей: этот слой берётся из кеша, пока не меняются go.mod и go.sum.
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO_ENABLED=0 даёт статический бинарник: в scratch нет libc, с которой он мог бы слинковаться.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/worker .

# Стадия запуска: scratch это пустой корень, в образе ровно один файл.
FROM scratch
COPY --from=build /out/worker /worker
# UID числом: в scratch нет /etc/passwd, а runAsNonRoot проверяется только по числовому UID.
USER 10001:10001
EXPOSE 8081
ENTRYPOINT ["/worker"]
