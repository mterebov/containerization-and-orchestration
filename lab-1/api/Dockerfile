# Стадия 1: сборка. Тут живёт toolchain Go, в итоговый образ он не попадёт.
FROM golang:1.22 AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /api .

# Стадия 2: запуск. scratch это пустой корень, в образе будет ровно один файл.
FROM scratch
COPY --from=build /api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
