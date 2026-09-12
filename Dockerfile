FROM golang:1.27-alpine AS build

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -o /out/mysqlBlackHole .
RUN CGO_ENABLED=0 go build -o /out/seed-redis ./cmd/seed-redis

FROM alpine:3.20 AS app

WORKDIR /app
COPY --from=build /out/mysqlBlackHole .
EXPOSE 3306
ENTRYPOINT ["./mysqlBlackHole"]

FROM alpine:3.20 AS seed

WORKDIR /app
COPY --from=build /out/seed-redis .
ENTRYPOINT ["./seed-redis"]