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
	log.Println("MySQL blackhole server listening on 0.0.0.0:3306")

	for {
		c, err := l.Accept()
		if err != nil {
			log.Println("accept error:", err)
			continue
		}

		go func() {
			conn, err := server.NewConn(c, "root", "", server.EmptyHandler{})
			if err != nil {
				log.Println("connection error:", err)
				c.Close()
				return
			}
			log.Printf("new connection id=%d user=%s\n", conn.ConnectionID(), conn.GetUser())

			for {
				if err := conn.HandleCommand(); err != nil {
					log.Printf("connection id=%d closed: %v\n", conn.ConnectionID(), err)
					return
				}
			}
		}()
	}
}
