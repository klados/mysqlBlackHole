package main

import (
	"log"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

type BlackHoleAuthHandler struct{}

func (h BlackHoleAuthHandler) OnAuthSuccess(conn *server.Conn) error {
	log.Println("Auth success for username:", conn.GetUser(), "from:", conn.RemoteAddr())
	return nil
}

func (h BlackHoleAuthHandler) OnAuthFailure(conn *server.Conn, err error) {
	log.Println("Auth failure for username:", conn.GetUser(), "from:", conn.RemoteAddr())
}

func (h BlackHoleAuthHandler) GetCredential(username string) (server.Credential, bool, error) {
	if username != "root" {
		return server.Credential{}, false, server.ErrAccessDenied
	}

	return server.Credential{
		Passwords:      []string{""},
		AuthPluginName: mysql.AUTH_NATIVE_PASSWORD,
	}, true, nil
}
