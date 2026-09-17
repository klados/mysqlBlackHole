package main

import (
	"context"
	"log/slog"
	"net"
	"os"

	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

const LevelSystem = slog.Level(2)

func systemLog(msg string, args ...any) {
	slog.Log(context.Background(), LevelSystem, msg, args...)
}

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.LevelKey && a.Value.Any() == LevelSystem {
				a.Value = slog.StringValue("SYSTEM")
			}
			return a
		},
	}))
	slog.SetDefault(logger.With(slog.String("service", "mysql-blackhole")))

	if err := godotenv.Load(); err != nil {
		systemLog("No .env file found, using defaults")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3306"
	}

	addr := "0.0.0.0:" + port
	l, err := net.Listen("tcp", addr)
	if err != nil {
		slog.Error("failed to listen", slog.Any("err", err))
		os.Exit(1)
	}
	systemLog("MySQL black hole server listening", slog.String("addr", addr))

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		slog.Error("failed to connect to redis", slog.String("addr", redisAddr), slog.Any("err", err))
		os.Exit(1)
	}
	systemLog("connected to redis", slog.String("addr", redisAddr))
	defer rdb.Close()

	srv := newServer()

	for {
		c, err := l.Accept()
		if err != nil {
			slog.Error("accept error", slog.Any("err", err))
			continue
		}

		go func() {
			handler := &BlackHoleHandler{redis: rdb}
			conn, err := srv.NewCustomizedConn(c, &BlackHoleAuthHandler{redis: rdb}, handler)
			if err != nil {
				c.Close()
				return
			}
			handler.SetUsername(conn.GetUser())
			handler.SetConnID(conn.ConnectionID())
			handler.SetFingerprint(GenerateFingerprint(conn))

			slog.Info("connection_open", handler.logAttrs()...)

			for {
				if err := conn.HandleCommand(); err != nil {
					slog.Info("connection_closed", handler.logAttrs(slog.Any("err", err))...)
					return
				}
			}
		}()
	}
}
