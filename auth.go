package main

import (
	"context"
	"errors"
	"log/slog"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
	"github.com/redis/go-redis/v9"

	"mysqlBlackHole/model/sqlemulate"
)

type BlackHoleAuthHandler struct {
	redis *redis.Client
}

func (h BlackHoleAuthHandler) OnAuthSuccess(conn *server.Conn) error {
	fp := GenerateFingerprint(conn)
	slog.Info("auth_success", fp.AttrsSlice()...)
	return nil
}

func (h BlackHoleAuthHandler) OnAuthFailure(conn *server.Conn, err error) {
	fp := GenerateFingerprint(conn)
	attrs := append(fp.AttrsSlice(), slog.Any("err", err))
	slog.Info("auth_failure", attrs...)
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
