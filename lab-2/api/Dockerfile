# Стадия 1: сборка. Тут живут toolchain Go и кеш модулей, в итоговый образ они не попадут.
FROM golang:1.27 AS build
WORKDIR /src
# Сначала только описание зависимостей: слой с модулями берётся из кеша,
# пока не меняются go.mod и go.sum, правки кода его не трогают.
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -o /api .

# Стадия 2: запуск. scratch это пустой корень, в образе будет ровно один файл.
FROM scratch
COPY --from=build /api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
