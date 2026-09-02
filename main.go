package main

import (
	"log"
	"net"

	"github.com/go-mysql-org/go-mysql/server"
)

func main() {
	l, err := net.Listen("tcp", "0.0.0.0:3306")
	if err != nil {
		log.Fatal(err)
	}
	log.Println("MySQL black hole server listening on 0.0.0.0:3306")

	srv := server.NewDefaultServer()

	for {
		c, err := l.Accept()
		if err != nil {
			log.Println("accept error:", err)
			continue
		}

		go func() {
			conn, err := srv.NewCustomizedConn(c, &BlackHoleAuthHandler{}, &BlackHoleHandler{})
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
