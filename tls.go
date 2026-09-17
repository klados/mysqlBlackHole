package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"os"
	"strings"
	"time"

	"github.com/go-mysql-org/go-mysql/mysql"
	"github.com/go-mysql-org/go-mysql/server"
)

// serverVersion is advertised in the initial handshake.
const serverVersion = "8.0.11"

// tlsEnabled reports whether the server should advertise CLIENT_SSL. It's on by
// default with a self-signed certificate; set TLS_ENABLED=false for plaintext-only.
func tlsEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("TLS_ENABLED")))
	return v == "" || v == "true" || v == "1" || v == "yes" || v == "on"
}

// newServer builds the MySQL server configuration. TLS is enabled by default via
// buildServerTLS, but never required, so plaintext clients keep working too.
func newServer() *server.Server {
	if !tlsEnabled() {
		systemLog("TLS disabled (TLS_ENABLED=false); client connections are plaintext")
		return server.NewServer(serverVersion, mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, nil)
	}

	tlsConf := buildServerTLS(os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE"))
	return server.NewServer(serverVersion, mysql.DEFAULT_COLLATION_ID, mysql.AUTH_NATIVE_PASSWORD, nil, tlsConf)
}

// buildServerTLS returns the server tls.Config. If certFile/keyFile are set they
// are loaded as-is; otherwise a self-signed cert is generated covering localhost,
// the loopback addresses, and any extra SANs from TLS_SANS.
func buildServerTLS(certFile, keyFile string) *tls.Config {
	if certFile != "" && keyFile != "" {
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			slog.Error("failed to load TLS certificate", slog.Any("err", err), slog.String("cert", certFile), slog.String("key", keyFile))
			os.Exit(1)
		}
		systemLog("TLS enabled with provided certificate", slog.String("cert", certFile))
		return &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	sans := append([]string{}, defaultSANS...)
	sans = append(sans, parseSANS(os.Getenv("TLS_SANS"))...)
	systemLog("TLS enabled with auto-generated self-signed certificate", slog.Any("sans", sans))
	return generateSelfSignedTLS(sans)
}

// defaultSANS are always present in the generated server certificate so local
// connections verify cleanly when verification is skipped or the cert is trusted.
var defaultSANS = []string{"localhost", "127.0.0.1", "::1"}

// parseSANS splits a comma-separated SAN list, trimming whitespace and blanks.
func parseSANS(raw string) []string {
	var sans []string
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			sans = append(sans, s)
		}
	}
	return sans
}

// generateSelfSignedTLS creates a throwaway CA and a server certificate signed by
// it. The cert is self-signed (untrusted by clients), so clients that verify peer
// certificates against a public CA will still reject it unless they skip
// verification or trust this CA.
func generateSelfSignedTLS(sans []string) *tls.Config {
	now := time.Now()

	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	ca := &x509.Certificate{
		SerialNumber:          randomSerial(),
		Subject:               pkix.Name{CommonName: "mysql-blackhole-ca"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		panic(err)
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	var dnsNames []string
	var ipAddrs []net.IP
	for _, s := range sans {
		if ip := net.ParseIP(s); ip != nil {
			ipAddrs = append(ipAddrs, ip)
		} else {
			dnsNames = append(dnsNames, s)
		}
	}
	leaf := &x509.Certificate{
		SerialNumber: randomSerial(),
		Subject:      pkix.Name{CommonName: "mysql-blackhole"},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dnsNames,
		IPAddresses:  ipAddrs,
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		panic(err)
	}

	leafPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER})
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)})

	cert, err := tls.X509KeyPair(append(leafPEM, caPEM...), keyPEM)
	if err != nil {
		panic(err)
	}

	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.NoClientCert,
	}
}

func randomSerial() *big.Int {
	const max = 128
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), max))
	if err != nil {
		panic(err)
	}
	return serial
}
