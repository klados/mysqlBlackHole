package main

import (
	"context"
	"errors"
	"log"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/redis/go-redis/v9"

	"mysqlBlackHole/model/sqlemulate"
)

type BlackHoleAuthHandler struct {
	redis *redis.Client
}

func (h BlackHoleAuthHandler) OnAuthSuccess(conn *server.Conn) error {
	log.Println("Auth success for username:", conn.GetUser(), "from:", conn.RemoteAddr())
	return nil
}

func (h BlackHoleAuthHandler) OnAuthFailure(conn *server.Conn, err error) {
	log.Println("Auth failure for username:", conn.GetUser(), "from:", conn.RemoteAddr())
}

func (h BlackHoleAuthHandler) GetCredential(username string) (server.Credential, bool, error) {
	password, err := h.redis.HGet(context.Background(), sqlemulate.KeyMySQLUsers, username).Result()
	if errors.Is(err, redis.Nil) {
		return server.Credential{}, false, server.ErrAccessDenied
	}
	if err != nil {
		return server.Credential{}, false, err
	}

	return server.Credential{
		Passwords:      []string{password},
		AuthPluginName: mysql.AUTH_NATIVE_PASSWORD,
	}, true, nil
}
