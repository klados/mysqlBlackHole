package main

import (
	"context"
	"log"
	"net"
	"os"

	"github.com/go-mysql-org/go-mysql/server"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found, using defaults")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3306"
	}

	addr := "0.0.0.0:" + port
	l, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("MySQL black hole server listening on %s\n", addr)

	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379"
	}

	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		log.Fatalf("failed to connect to redis at %s: %v", redisAddr, err)
	}
	log.Printf("connected to redis at %s", redisAddr)
	defer rdb.Close()

	srv := server.NewDefaultServer()

	for {
		c, err := l.Accept()
		if err != nil {
			log.Println("accept error:", err)
			continue
		}

		go func() {
			conn, err := srv.NewCustomizedConn(c, &BlackHoleAuthHandler{redis: rdb}, &BlackHoleHandler{})
			if err != nil {
				c.Close()
				return
			}

			for {
				if err := conn.HandleCommand(); err != nil {
					log.Printf("connection id=%d closed: %v\n", conn.ConnectionID(), err)
					return
				}
			}
		}()
	}
}
