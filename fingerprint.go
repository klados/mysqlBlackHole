package main

import (
	"crypto/sha256"
	"crypto/tls"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"

	"github.com/go-mysql-org/go-mysql/server"
)

type ClientFingerprint struct {
	Hash       string
	IP         string
	Username   string
	Attrs      map[string]string
	TLSSubject string
}

func GenerateFingerprint(conn *server.Conn) ClientFingerprint {
	remoteIP, _, _ := net.SplitHostPort(conn.RemoteAddr().String())
	username := conn.GetUser()
	attrs := conn.Attributes()

	tlsSubject := ""
	if tc, ok := conn.Conn.Conn.(*tls.Conn); ok {
		if state := tc.ConnectionState(); len(state.PeerCertificates) > 0 {
			tlsSubject = state.PeerCertificates[0].Subject.CommonName
		}
	}

	fp := ClientFingerprint{
		IP:         remoteIP,
		Username:   username,
		Attrs:      attrs,
		TLSSubject: tlsSubject,
	}
	fp.Hash = computeHash(fp)
	return fp
}

func computeHash(fp ClientFingerprint) string {
	var b strings.Builder
	b.WriteString(fp.IP)
	b.WriteString(":")
	b.WriteString(fp.Username)
	b.WriteString(":")
	b.WriteString(fp.TLSSubject)
	b.WriteString(":")

	keys := make([]string, 0, len(fp.Attrs))
	for k := range fp.Attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		b.WriteString(k)
		b.WriteString("=")
		b.WriteString(fp.Attrs[k])
		b.WriteString(";")
	}

	sum := sha256.Sum256([]byte(b.String()))
	return fmt.Sprintf("%x", sum[:8])
}

func (fp ClientFingerprint) AttrsSlice() []any {
	return []any{
		slog.String("fp", fp.Hash),
		slog.String("ip", fp.IP),
		slog.String("user", fp.Username),
	}
}
